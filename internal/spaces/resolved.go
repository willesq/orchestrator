package spaces

import "slices"

// Resolved is a validated policy, ready for lookups. A nil *Resolved is no
// policy at all: one implicit Shared space that every caller reads.
type Resolved struct {
	Policy Policy
	// PublicRootSessions is where content from sessions under a public root
	// goes: a private or isolated space, or Quarantine.
	PublicRootSessions string
	spaces             map[string]*Space
	// roots are every bound folder root, expanded, for placement.
	roots []root
	// publicPaths are each public space's public_paths, parsed, by space.
	publicPaths map[string][]publicPath
	// ghDirs are each space's gh_config_dir, expanded, by space.
	ghDirs map[string]string
	// claudeDirs are each space's claude_config_dir, expanded, by space.
	claudeDirs map[string]string
}

// Configured reports whether any space is defined. Without one, orch keeps
// everything in Shared and behaves as it did before spaces.
func (r *Resolved) Configured() bool {
	return r != nil && len(r.spaces) > 0
}

// Space returns the named space.
func (r *Resolved) Space(name string) (Space, bool) {
	if r == nil {
		return Space{}, false
	}
	s, ok := r.spaces[name]
	if !ok {
		return Space{}, false
	}
	return *s, true
}

// CanRead reports whether a caller in space reader may read content in space
// target. A space reads itself, every public space and the spaces it lists;
// nothing reads Quarantine. Validate has already refused reads that cross
// isolated spaces, so a listed read is allowed as listed.
func (r *Resolved) CanRead(reader, target string) bool {
	if !r.Configured() {
		return reader == Shared && target == Shared
	}
	if target == Quarantine {
		return false
	}
	from, ok := r.spaces[reader]
	if !ok {
		return false
	}
	to, ok := r.spaces[target]
	if !ok {
		return false
	}
	return reader == target || to.Kind == Public || slices.Contains(from.Reads, target)
}

// ReadableFrom lists every public space and each space reader may read,
// sorted. A reader the policy does not name (none, Quarantine, or a space a
// later policy dropped) reads public spaces only.
func (r *Resolved) ReadableFrom(reader string) []string {
	out := []string{}
	if r == nil {
		return out
	}
	for _, sp := range r.Policy.Spaces {
		if sp.Kind == Public || (reader != "" && r.CanRead(reader, sp.Name)) {
			out = append(out, sp.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// GitHubOrgSpace returns the space GitHub account login is bound to.
// Logins match case-insensitively, as GitHub treats them.
func (r *Resolved) GitHubOrgSpace(login string) (string, bool) {
	if !r.Configured() {
		return "", false
	}
	k := orgKey(login)
	for _, s := range r.Policy.Spaces {
		if slices.ContainsFunc(s.GitHubOrgs, func(org string) bool { return orgKey(org) == k }) {
			return s.Name, true
		}
	}
	return "", false
}
