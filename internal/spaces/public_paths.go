package spaces

import (
	"path"
	"path/filepath"
	"strings"
)

// public_paths (SP-076, D-65) list, per public space, the local folders whose
// content may reach that space. A pattern has three forms only:
//
//   - an exact folder, "~/code/x": the files directly in it;
//   - "*" inside one folder name, "~/code/*/docs": one folder name, matched
//     with path.Match;
//   - "**" as a whole folder name, "~/code/x/**": any depth, zero or more
//     folders.
//
// Nothing else is accepted: no "!" negation, no "?", "[...]" or "{...}", no
// escapes, and no ".", ".." or empty folder names. Reason: a pattern decides
// what every session can read, so its meaning must be plain from its text.

// segKind is what one folder name of a pattern matches.
type segKind int

const (
	segLiteral segKind = iota // the name itself, case-insensitively
	segGlob                   // one name, matched with path.Match
	segAny                    // "**": zero or more names
)

type pathSeg struct {
	kind segKind
	// name is case-folded: macOS volumes are usually case-insensitive, as
	// roots assume.
	name string
}

// publicPath is one public_paths pattern, parsed.
type publicPath struct {
	// pattern is the entry as written, for explanations.
	pattern string
	segs    []pathSeg
}

// forbiddenPatternChars are glob and gitignore syntax D-65 leaves out.
const forbiddenPatternChars = `!?[]{}\`

// parsePublicPath parses one pattern, expanding a leading "~" to home. It
// returns why the pattern is refused, or "". The reason never repeats the
// pattern, so an error cannot echo a term a folder name holds.
func parsePublicPath(pattern, home string) (publicPath, string) {
	if pattern == "" {
		return publicPath{}, "is empty"
	}
	if strings.HasPrefix(pattern, "!") {
		return publicPath{}, "starts with '!'; public_paths has no negation, so list only the folders that are public"
	}
	if strings.ContainsAny(pattern, forbiddenPatternChars) {
		return publicPath{}, "uses one of ! ? [ ] { } \\; only exact folders, '*' within a folder name and '**' are allowed"
	}
	var segs []pathSeg
	rest := pattern
	switch {
	case pattern == "~" || strings.HasPrefix(pattern, "~/"):
		if home == "" || !filepath.IsAbs(home) {
			return publicPath{}, "starts with ~, but the home folder is unknown"
		}
		// Reason: the home folder's own name is literal even if it holds a
		// '*', so only what the operator wrote after ~ can be a wildcard.
		for _, name := range splitPath(filepath.Clean(home)) {
			segs = append(segs, pathSeg{segLiteral, strings.ToLower(name)})
		}
		rest = strings.TrimPrefix(pattern, "~")
		if rest == "" {
			return publicPath{pattern: pattern, segs: segs}, ""
		}
	case !strings.HasPrefix(pattern, "/"):
		return publicPath{}, "is not an absolute path (start it with / or ~/)"
	}
	if rest != "/" && path.Clean(rest) != rest {
		return publicPath{}, "is not a clean path (no '.', '..', '//' or trailing '/')"
	}
	for _, name := range splitPath(rest) {
		switch {
		case name == "**":
			segs = append(segs, pathSeg{segAny, ""})
		case strings.Contains(name, "**"):
			return publicPath{}, "uses '**' inside a folder name; '**' must be a whole folder name"
		case strings.Contains(name, "*"):
			segs = append(segs, pathSeg{segGlob, strings.ToLower(name)})
		default:
			segs = append(segs, pathSeg{segLiteral, strings.ToLower(name)})
		}
	}
	return publicPath{pattern: pattern, segs: segs}, ""
}

// splitPath splits a clean absolute path into its folder names.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// fixedPrefix is the folder the pattern's literal names lead to, before any
// wildcard: every folder the pattern matches lies at or under it.
func (pp publicPath) fixedPrefix() string {
	var names []string
	for _, s := range pp.segs {
		if s.kind != segLiteral {
			break
		}
		names = append(names, s.name)
	}
	return "/" + strings.Join(names, "/")
}

// matchesDir reports whether the pattern matches folder dir, given as its
// case-folded folder names.
func (pp publicPath) matchesDir(dir []string) bool {
	// ok[j] reports whether the pattern's segments so far match dir[:j].
	// Reason: a table, not recursion, keeps the cost at the pattern's
	// length times the path's depth, however many "**" it holds.
	ok := make([]bool, len(dir)+1)
	ok[0] = true
	for _, s := range pp.segs {
		next := make([]bool, len(dir)+1)
		for j := range ok {
			if !ok[j] {
				continue
			}
			if s.kind == segAny {
				// Reason: every later position follows from the first,
				// so each "**" costs one pass over the path, not one per
				// position.
				for k := j; k <= len(dir); k++ {
					next[k] = true
				}
				break
			}
			switch s.kind {
			case segLiteral:
				if j < len(dir) && dir[j] == s.name {
					next[j+1] = true
				}
			case segGlob:
				if j < len(dir) {
					if m, err := path.Match(s.name, dir[j]); err == nil && m {
						next[j+1] = true
					}
				}
			}
		}
		ok = next
	}
	return ok[len(dir)]
}

// PublicPathFor returns the public_paths pattern of space, as written, that
// makes content at p public: for a file, a pattern matching its folder; for
// a folder (isDir), a pattern matching the folder itself, which is what the
// files directly in it get. The first matching pattern in the policy's order
// is returned.
func (r *Resolved) PublicPathFor(space, p string, isDir bool) (string, bool) {
	if r == nil || p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	pats := r.publicPaths[space]
	if len(pats) == 0 {
		return "", false
	}
	dir := filepath.Clean(p)
	if !isDir {
		dir = filepath.Dir(dir)
	}
	names := splitPath(strings.ToLower(filepath.ToSlash(dir)))
	for _, pp := range pats {
		if pp.matchesDir(names) {
			return pp.pattern, true
		}
	}
	return "", false
}

// checkPublicPaths parses every space's public_paths (D-65): only a public
// space lists them, each must parse, and the deepest root holding the folder
// its literal names lead to must be one of that space's roots, so a pattern
// lies inside its own public root and is not shadowed by a deeper root that
// decides first. Problems name the space and the entry's position, never
// the pattern.
func checkPublicPaths(names []string, byName map[string]*Space, roots []root, home string, add func(Rule, string, string, ...any)) map[string][]publicPath {
	out := map[string][]publicPath{}
	for _, n := range names {
		s := byName[n]
		if len(s.PublicPaths) == 0 {
			continue
		}
		if s.Kind != Public {
			add(RuleBadPublicPath, n, "space %q is %s; only a public space lists public_paths", n, s.Kind)
			continue
		}
		for i, pattern := range s.PublicPaths {
			pp, why := parsePublicPath(pattern, home)
			if why != "" {
				add(RuleBadPublicPath, n, "space %q: public_paths entry %d %s", n, i+1, why)
				continue
			}
			if deep, ok := deepestRoot(pp.fixedPrefix(), roots); !ok || deep.space != n {
				add(RulePublicPathOutsideRoot, n, "space %q: public_paths entry %d is not inside one of its roots (or a deeper root of another space holds it)", n, i+1)
				continue
			}
			out[n] = append(out[n], pp)
		}
	}
	return out
}

// deepestRoot is the deepest root holding folder dir (case-folded).
func deepestRoot(dir string, roots []root) (root, bool) {
	var best root
	found := false
	for _, r := range roots {
		if dir == r.key || strings.HasPrefix(dir, strings.TrimSuffix(r.key, "/")+"/") {
			if !found || len(r.key) > len(best.key) {
				best, found = r, true
			}
		}
	}
	return best, found
}
