package feeds

import (
	"slices"
	"strings"
)

// For returns what a lists for one tenant, named as a policy binds it
// ("<system>:<id>", matched regardless of case): whether its DMs and group
// DMs are stored, and the containers stored. A connector reads only these,
// so a conversation taken off the list is no longer fetched at all.
func (a Allowlist) For(tenantKey string) (directMessages bool, containers []string) {
	key := strings.ToLower(tenantKey)
	for _, t := range a.DirectMessages {
		if strings.ToLower(t) == key {
			directMessages = true
		}
	}
	for t, ids := range a.Containers {
		if strings.ToLower(t) == key {
			containers = append(containers, ids...)
		}
	}
	slices.Sort(containers)
	return directMessages, slices.Compact(containers)
}

// ValidID reports whether v is a stable ID a record may hold.
func ValidID(v string) bool { return idPattern.MatchString(v) }
