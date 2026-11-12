package spaces

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Self-protection (spaces plan SP-042): the guard refuses agent writes to
// what keeps the guard and the other layers working (orch's config and
// data, the guard's own files, harness settings and hooks, git hooks, terms
// files) and refuses even reads of terms files and credentials. Otherwise
// an agent could remove orch's hooks, edit its pin or switch the guard off.

// Protection is what the guard protects.
type Protection struct {
	// Write are absolute paths no call may write. A directory protects
	// everything below it, and a call that writes a directory holding one
	// (rm -rf, mv) writes it too. A final element holding '*' is a name
	// pattern: ~/.claude/settings* is every file named settings... there.
	Write []string
	// Anywhere are relative paths protected below every directory, such
	// as .git/hooks or .claude/settings*: a checkout's own harness settings
	// and git hooks. A call that writes a directory they would be under,
	// such as a .git or .claude folder itself, writes them too.
	Anywhere []string
	// Read are absolute paths no call may read or write: terms files and
	// credentials. Name patterns work as in Write.
	Read []string
}

// ProtectedPaths is the protection orch applies, for orchd's guard and for
// a replay of the command log (SP-047), so both judge the same paths: orch's
// config and data directories, harness settings, git hooks, each space's
// terms file and gh profile, and credentials. home, configDir and dataDir
// are absolute; res may be nil.
func ProtectedPaths(home, configDir, dataDir string, res *Resolved) Protection {
	p := Protection{
		Write: []string{
			configDir, dataDir,
			filepath.Join(home, ".claude", "settings*"),
			filepath.Join(home, ".codex", "hooks.json"),
			filepath.Join(home, ".codex", "config.toml"),
			filepath.Join(home, ".gemini", "settings.json"),
		},
		Anywhere: []string{
			filepath.Join(".git", "hooks"),
			// Reason: core.hooksPath or core.fsmonitor set there run
			// commands as surely as a hook does.
			filepath.Join(".git", "config"),
			filepath.Join(".claude", "settings*"),
			filepath.Join(".codex", "hooks.json"),
			filepath.Join(".codex", "config.toml"),
			filepath.Join(".gemini", "settings.json"),
		},
		Read: []string{
			filepath.Join(dataDir, "qdrant-api-key"),
			// Reason: every file of a gh profile, whose login it holds; gh
			// reads them itself, an agent never needs to. The directory
			// itself stays nameable, as GH_CONFIG_DIR=<dir> gh ... does.
			filepath.Join(home, ".config", "gh", "*"),
			filepath.Join(home, ".netrc"),
			filepath.Join(home, ".aws", "credentials"),
			filepath.Join(home, ".ssh", "id_*"),
		},
	}
	if !res.Configured() {
		return p
	}
	for _, s := range res.Policy.Spaces {
		if s.TermsFile != "" {
			if t := (Env{Home: home}).expand(s.TermsFile); t != "" {
				p.Read = append(p.Read, literalName(t))
			}
		}
		if d := res.ghDirs[s.Name]; d != "" {
			p.Read = append(p.Read, filepath.Join(d, "*"))
		}
	}
	return p
}

// literalName escapes the name pattern characters in p's final element, so
// a path such as ~/terms/job[1].txt is protected as written.
func literalName(p string) string {
	dir, base := filepath.Split(p)
	var b strings.Builder
	for _, r := range base {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return dir + b.String()
}

// pathPattern is one protected entry: a path (every spelling of it), or a
// directory with a name pattern for its entries.
type pathPattern struct {
	keys []string
	// name, when set, is a pattern the final element must match; keys are
	// then its directory.
	name string
}

// match reports whether key is the entry or lies below it.
func (p pathPattern) match(key string) bool {
	for _, k := range p.keys {
		if p.name == "" {
			if under(key, k) {
				return true
			}
			continue
		}
		rest, ok := strings.CutPrefix(key, strings.TrimSuffix(k, "/")+"/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		if p.excluded(first) {
			continue
		}
		if ok, err := filepath.Match(p.name, first); ok || err != nil && p.name == first {
			return true
		}
	}
	return false
}

// dirOf is the directory a prefix must lie in to reach the entry: a
// pattern's directory, or a plain entry itself (a prefix below a protected
// directory, or the protected file).
func (p pathPattern) dirOf() []string { return p.keys }

// excluded reports names a pattern does not cover: public keys next to
// the private ones id_* protects.
func (p pathPattern) excluded(name string) bool {
	return p.name != "*" && strings.HasSuffix(name, ".pub")
}

// within reports whether the entry lies at or below key: a call writing
// key (rm -rf, mv) writes the entry too.
func (p pathPattern) within(key string) bool {
	for _, k := range p.keys {
		if under(k, key) {
			return true
		}
	}
	return false
}

// selfProtection is the part of a GuardPolicy SP-042 adds.
type selfProtection struct {
	write, read []pathPattern
	// readDirs are, per read entry, the directories a prefix must lie in
	// to reach it: a pattern's directory, a plain entry's parent.
	readDirs [][]string
	// anywhere are relative patterns, split into lower-case elements.
	anywhere [][]string
}

// NewGuardPolicyProtecting is NewGuardPolicy with the whole of p: write
// and read protection, name patterns and paths protected below every
// directory. Every Write and Read entry must be absolute.
func NewGuardPolicyProtecting(res *Resolved, p Protection) (GuardPolicy, error) {
	for _, list := range [][]string{p.Write, p.Read} {
		for _, e := range list {
			if !filepath.IsAbs(e) {
				return GuardPolicy{}, fmt.Errorf("protected path %q is not absolute", e)
			}
		}
	}
	for _, a := range p.Anywhere {
		if a == "" || filepath.IsAbs(a) {
			return GuardPolicy{}, fmt.Errorf("path %q protected anywhere must be relative", a)
		}
	}
	g, err := NewGuardPolicy(res, nil)
	if err != nil || !res.Configured() {
		return g, err
	}
	pr := newPathResolver()
	patterns := func(list []string) []pathPattern {
		var out []pathPattern
		for _, e := range list {
			dir, name := e, ""
			if base := filepath.Base(e); strings.ContainsAny(base, "*?[") {
				dir, name = filepath.Dir(e), foldName(base)
			}
			out = append(out, pathPattern{keys: spellings(pr, dir), name: name})
		}
		return out
	}
	g.self.write, g.self.read = patterns(p.Write), patterns(p.Read)
	for _, r := range g.self.read {
		g.self.readDirs = append(g.self.readDirs, r.dirOf())
	}
	for _, a := range p.Anywhere {
		g.self.anywhere = append(g.self.anywhere, strings.Split(guardKey(a), "/"))
	}
	return g, nil
}

// selfProtected reports whether the call writes a protected path or reads
// a terms file or credential.
func (pol GuardPolicy) selfProtected(tool string, t ToolTargets) bool {
	writes := t.Writes
	if len(writes) == 0 && writeTools[tool] {
		// Reason: targets recorded before SP-042 have no Writes; a file
		// tool's paths are then what it writes.
		writes = append(append(append(append([]string(nil), t.Paths...), t.CdDest...), t.GitDirs...), t.Prefixes...)
	}
	for _, w := range writes {
		k := guardKey(w)
		if pol.anyProtected([]string{w}) || pol.self.writes(k) {
			return true
		}
		// A write to a directory holding a protected path (rm -rf ~/.config).
		for _, keys := range pol.protected {
			if (pathPattern{keys: keys}).within(k) {
				return true
			}
		}
	}
	for _, list := range [][]string{t.Paths, t.CdDest, t.GitDirs, t.Prefixes, writes} {
		for _, p := range list {
			if pol.self.reads(guardKey(p)) {
				return true
			}
		}
	}
	// A prefix (a glob or an unknown word) at or below the directory of a
	// read-protected entry: cat ~/.ssh/*. A prefix further up (ls ~/*) is
	// a listing of the home, not a read of what it holds.
	for _, p := range t.Prefixes {
		k := guardKey(p)
		for _, dirs := range pol.self.readDirs {
			for _, d := range dirs {
				if under(k, d) {
					return true
				}
			}
		}
	}
	return false
}

// writes reports whether writing key changes a protected path.
func (s selfProtection) writes(key string) bool {
	for _, p := range s.write {
		if p.match(key) || p.within(key) {
			return true
		}
	}
	parts := strings.Split(key, "/")
	for _, a := range s.anywhere {
		for i := range parts {
			if anywhereAt(parts[i:], a) {
				return true
			}
		}
		// The folder the pattern starts in, itself: rm -rf .git, mv .claude.
		if len(a) > 1 && parts[len(parts)-1] == a[0] {
			return true
		}
	}
	return false
}

// reads reports whether key is a terms file or credential, or lies below
// one.
func (s selfProtection) reads(key string) bool {
	for _, p := range s.read {
		if p.match(key) {
			return true
		}
	}
	return false
}

// anywhereAt reports whether parts starts with pattern a, the last element
// of a matched as a name pattern, and possibly continues below it.
func anywhereAt(parts, a []string) bool {
	if len(parts) < len(a) {
		return false
	}
	for i, el := range a {
		if i == len(a)-1 {
			ok, _ := filepath.Match(el, parts[i])
			return ok
		}
		if parts[i] != el {
			return false
		}
	}
	return false
}
