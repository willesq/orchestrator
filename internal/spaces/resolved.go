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
