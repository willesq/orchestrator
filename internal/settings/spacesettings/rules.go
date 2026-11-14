// Package spacesettings writes per-space harness settings (spaces plan
// SP-046, D-23, D-53): the OS sandbox and file-tool denies that keep one
// job's sessions out of another job's folders, the space's gh profile
// (GH_CONFIG_DIR, only when the space names one, D-59) and its own
// temporary directory. orchd keeps them in every
// checkout's .claude/settings.local.json under a space's roots; 'orch run'
// passes them to a session started anywhere. See docs/internals/install.md
// "Per-space harness settings".
package spacesettings

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Layout places orch's per-space directories: <Dir>/<space> is the space's
// folder, and TmpDir its TMPDIR. Each space's directories are denied to
// every other space, so temp files do not cross between them.
type Layout struct {
	Dir string
	// Tmp holds every space's temp folder; "" keeps it at
	// <Dir>/<space>/tmp. Reason for a separate root: Claude Code uses a
	// CLAUDE_CODE_TMPDIR only when it is short (claudeTmpMax), which a
	// folder in orch's data directory is not.
	Tmp string
}

// claudeTmpMax is the longest temp folder Claude Code takes as given.
// Measured with Claude Code 2.1.309 (RC-184): its sandboxed commands get
// TMPDIR=<CLAUDE_CODE_TMPDIR>/claude-<uid> when that path is at most 44
// bytes (the AF_UNIX socket limit it keeps under), and otherwise
// /tmp/claude-<uid>, which every session of the user shares. Only the
// process environment sets CLAUDE_CODE_TMPDIR: a settings file's env does
// not change it.
const claudeTmpMax = 44

// DefaultLayout keeps the per-space directories in orch's data directory
// and the temp folders under /tmp/orch-<uid>, short enough for Claude Code.
func DefaultLayout() Layout {
	return Layout{Dir: filepath.Join(paths.DataDir(), "spaces"), Tmp: fmt.Sprintf("/tmp/orch-%d", os.Getuid())}
}

// SpaceDir is the space's folder under Dir: its profile, and its temp
// folder when Tmp is "".
func (l Layout) SpaceDir(space string) string { return filepath.Join(l.Dir, space) }

// TmpDir is the space's TMPDIR: <Tmp>/<space>, or <Tmp>/_<hash> when the
// name would make Claude Code's folder in it too long. Space names start
// with a letter, so no name is like a hash's.
func (l Layout) TmpDir(space string) string {
	if l.Tmp == "" {
		return filepath.Join(l.SpaceDir(space), "tmp")
	}
	if d := filepath.Join(l.Tmp, space); len(claudeUIDDir(d)) <= claudeTmpMax {
		return d
	}
	sum := sha256.Sum256([]byte(space))
	// 48 bits keep the folder within claudeTmpMax for uids up to 5 digits.
	return filepath.Join(l.Tmp, "_"+hex.EncodeToString(sum[:6]))
}

// EnsureTmpDir creates the space's temp folder owner-only, and the Tmp root
// first: under the shared /tmp, a root another user made or linked is
// refused rather than used.
func (l Layout) EnsureTmpDir(space string) error {
	if l.Tmp != "" {
		if err := EnsurePrivateDir(l.Tmp); err != nil {
			return err
		}
	}
	return EnsurePrivateDir(l.TmpDir(space))
}

// claudeUIDDir is Claude Code's own folder in a temp folder.
func claudeUIDDir(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("claude-%d", os.Getuid()))
}

// SharedClaudeTmp is the TMPDIR Claude Code gives sandboxed commands when it
// runs without a usable CLAUDE_CODE_TMPDIR: one folder for every session of
// the user, whatever its space.
func SharedClaudeTmp() string { return claudeUIDDir("/tmp") }

// ClaudeSandboxTmp is the TMPDIR Claude Code gives sandboxed commands of a
// session started with CLAUDE_CODE_TMPDIR=dir in its environment.
func ClaudeSandboxTmp(dir string) string {
	if p := claudeUIDDir(dir); len(p) <= claudeTmpMax {
		return p
	}
	return SharedClaudeTmp()
}

// OrchDir holds orchd's own record of the checkout files and their backups.
// It is denied to every space: the record names every space's checkouts
// and the backups hold users' own settings. Space names start with a
// letter, so no space's folder is named like it.
func (l Layout) OrchDir() string { return filepath.Join(l.Dir, ".orch") }

// Rules is what a session in one space gets.
type Rules struct {
	// Space is the space the rules are for; "" for a checkout that has no
	// space (a conflict or quarantine), which gets only denies.
	Space string
	// Deny lists absolute folders the session may neither read nor write:
	// other isolated spaces' roots and every other space's directory under
	// the layout. Sorted, without duplicates.
	Deny []string
	// TmpDir is the space's TMPDIR; "" when it has none.
	TmpDir string
	// GHConfigDir is the space's gh profile; "" for the default login.
	GHConfigDir string
	// ClaudeConfigDir is the space's Claude Code folder for 'orch run';
	// "" for the user's own (D-59).
	ClaudeConfigDir string
}

// For returns the rules of a session in space. It denies every isolated
// root of another space, every other space's directory and gh profile, and
// orchd's own folder; and nothing of space itself, of a public space or of
// a private space space reads: those are left to memory filtering and the
// tool guard, as in block-other-jobs mode (D-12).
func For(res *spaces.Resolved, space string, l Layout) Rules {
	r := Rules{Space: space, TmpDir: l.TmpDir(space), GHConfigDir: res.GHConfigDir(space), ClaudeConfigDir: res.ClaudeConfigDir(space)}
	deny := []string{l.OrchDir()}
	for _, root := range res.BoundRoots() {
		if root.Kind == spaces.Isolated && root.Space != space {
			deny = append(deny, root.Root)
		}
	}
	for _, n := range res.Names() {
		if n != space {
			deny = append(deny, otherSpace(res, n, l)...)
		}
	}
	r.Deny = withReal(deny)
	return r
}

// otherSpace is what of space n another space's sessions are denied besides
// its roots: its folders under the layout, its gh profile and its Claude
// Code folder. Reason: a gh profile can hold the account's token in
// hosts.yml, and GH_CONFIG_DIR pointed at it acts as that account; a Claude
// Code folder holds its login and the space's transcripts.
func otherSpace(res *spaces.Resolved, n string, l Layout) []string {
	out := []string{l.SpaceDir(n)}
	if l.Tmp != "" {
		out = append(out, l.TmpDir(n))
	}
	if gh := res.GHConfigDir(n); gh != "" {
		out = append(out, gh)
	}
	if cl := res.ClaudeConfigDir(n); cl != "" {
		out = append(out, cl)
	}
	return out
}

// ForUnplaced returns the rules of a checkout at path that has no space: its
// root and its remote name different jobs, or it is quarantined. It denies
// every isolated root that does not hold path, every space's directory, gh
// profile and Claude Code folder, and orchd's own folder, and sets no gh
// profile or TMPDIR.
func ForUnplaced(res *spaces.Resolved, path string, l Layout) Rules {
	deny := []string{l.OrchDir()}
	for _, root := range res.BoundRoots() {
		if root.Kind == spaces.Isolated && !spaces.RootHolds(root.Root, path) {
			deny = append(deny, root.Root)
		}
	}
	for _, n := range res.Names() {
		deny = append(deny, otherSpace(res, n, l)...)
	}
	return Rules{Deny: withReal(deny)}
}

// realPath resolves p's links. A part of p that does not exist yet (a
// space's temp folder before its first use) is kept as written below its
// nearest existing parent's real path.
func realPath(p string) (string, bool) {
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(real, rest), true
		}
		if filepath.Dir(d) == d {
			return "", false
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// withReal adds each path's real form when a symlink leads to it, then sorts
// and removes duplicates. Reason: the sandbox matches the path a command
// opens after the kernel resolves links (/tmp is /private/tmp on macOS),
// while a permission rule matches the path a tool names.
func withReal(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		p = filepath.Clean(p)
		out = append(out, p)
		if real, ok := realPath(p); ok && real != p {
			out = append(out, real)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// globMeta are characters with a meaning in Claude Code's permission rule
// paths. A folder whose path holds one cannot be named exactly in a rule.
const globMeta = "*?[]{}()!\\"

// RuleSafe reports whether path can be written into a Read(...) or Edit(...)
// permission rule as itself.
func RuleSafe(path string) bool { return !strings.ContainsAny(path, globMeta) }
