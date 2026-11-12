package spaces

import (
	"sort"
	"strings"
)

// Lookups for per-space harness settings (SP-046), which deny each space's
// sessions the folders of other jobs.

// BoundRoots returns every bound folder root, expanded, with its space and
// kind, sorted by path. Nil without a policy.
func (r *Resolved) BoundRoots() []RootMatch {
	if !r.Configured() {
		return nil
	}
	out := make([]RootMatch, 0, len(r.roots))
	for _, rt := range r.roots {
		out = append(out, RootMatch{Root: rt.path, Space: rt.space, Kind: rt.kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out
}

// Names returns every space's name, sorted. Nil without a policy.
func (r *Resolved) Names() []string {
	if !r.Configured() {
		return nil
	}
	out := make([]string, 0, len(r.spaces))
	for n := range r.spaces {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// GHConfigDir returns the space's gh_config_dir, expanded; "" when the space
// names none, so its GitHub calls use the default gh login.
func (r *Resolved) GHConfigDir(space string) string {
	if !r.Configured() {
		return ""
	}
	return r.ghDirs[space]
}

// RootHolds reports whether root, a bound root as BoundRoots returns it,
// holds path or is path. Paths compare case-insensitively, as placement
// does, since macOS volumes usually are.
func RootHolds(root, path string) bool {
	k, p := strings.ToLower(root), strings.ToLower(path)
	return p == k || strings.HasPrefix(p, strings.TrimSuffix(k, "/")+"/")
}
