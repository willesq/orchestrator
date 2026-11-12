// Package spacesettings writes per-space harness settings (spaces plan
// SP-046, D-23, D-53): the OS sandbox and file-tool denies that keep one
// job's sessions out of another job's folders, the space's gh profile
// (GH_CONFIG_DIR) and its own temporary directory. orchd keeps them in every
// checkout's .claude/settings.local.json under a space's roots; 'orch run'
// passes them to a session started anywhere. See docs/internals/install.md
// "Per-space harness settings".
package spacesettings

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Layout places orch's per-space directories: <Dir>/<space>/tmp is the
// space's TMPDIR and <Dir>/<space>/claude its Claude Code profile for
// 'orch run'. Each space's directory is denied to every other space, so
// neither temp files nor a profile's transcripts cross between them.
type Layout struct{ Dir string }

// DefaultLayout keeps the per-space directories in orch's data directory.
func DefaultLayout() Layout { return Layout{Dir: filepath.Join(paths.DataDir(), "spaces")} }

// SpaceDir is everything orch keeps for one space.
func (l Layout) SpaceDir(space string) string { return filepath.Join(l.Dir, space) }

// TmpDir is the space's TMPDIR.
func (l Layout) TmpDir(space string) string { return filepath.Join(l.SpaceDir(space), "tmp") }

// ClaudeDir is the space's CLAUDE_CONFIG_DIR for 'orch run'.
func (l Layout) ClaudeDir(space string) string { return filepath.Join(l.SpaceDir(space), "claude") }

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
}

// For returns the rules of a session in space. It denies every isolated
// root of another space, every other space's directory and gh profile, and
// orchd's own folder; and nothing of space itself, of a public space or of
// a private space space reads: those are left to memory filtering and the
// tool guard, as in block-other-jobs mode (D-12).
func For(res *spaces.Resolved, space string, l Layout) Rules {
	r := Rules{Space: space, TmpDir: l.TmpDir(space), GHConfigDir: res.GHConfigDir(space)}
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
// its roots: its folder under the layout and its gh profile. Reason: a gh
// profile can hold the account's token in hosts.yml, and GH_CONFIG_DIR
// pointed at it acts as that account.
func otherSpace(res *spaces.Resolved, n string, l Layout) []string {
	out := []string{l.SpaceDir(n)}
	if gh := res.GHConfigDir(n); gh != "" {
		out = append(out, gh)
	}
	return out
}

// ForUnplaced returns the rules of a checkout at path that has no space: its
// root and its remote name different jobs, or it is quarantined. It denies
// every isolated root that does not hold path, every space's directory and
// gh profile, and orchd's own folder, and sets no gh profile or TMPDIR.
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

// withReal adds each path's real form when a symlink leads to it, then sorts
// and removes duplicates. Reason: the sandbox matches the path a command
// opens after the kernel resolves links (/tmp is /private/tmp on macOS),
// while a permission rule matches the path a tool names.
func withReal(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		p = filepath.Clean(p)
		out = append(out, p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
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
