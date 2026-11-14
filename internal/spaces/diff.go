package spaces

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Change is how one space differs between two policies.
type Change struct {
	Space string
	// Kind is "added", "removed" or "changed".
	Kind string
	// Fields lists what changed, for "changed".
	Fields []string
	// Details shows the values: one line per field changed, or the whole
	// space when it was added or removed, so a reviewer sees what a draft
	// does and not only where.
	Details []string
}

// Compare lists the spaces that differ between from and to, by name, plus a
// change to public_root_sessions under the space name "".
func Compare(from, to Policy) []Change {
	index := func(p Policy) map[string]Space {
		m := make(map[string]Space, len(p.Spaces))
		for _, s := range p.Spaces {
			m[s.Name] = s
		}
		return m
	}
	a, b := index(from), index(to)
	names := make([]string, 0, len(a)+len(b))
	for n := range a {
		names = append(names, n)
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []Change
	if from.PublicRootSessions != to.PublicRootSessions {
		out = append(out, Change{Kind: "changed", Fields: []string{"public_root_sessions"},
			Details: []string{fmt.Sprintf("public_root_sessions: %q -> %q", from.PublicRootSessions, to.PublicRootSessions)}})
	}
	for _, n := range names {
		x, inA := a[n]
		y, inB := b[n]
		switch {
		case !inA:
			out = append(out, Change{Space: n, Kind: "added", Details: changedFields(Space{}, y).details})
		case !inB:
			out = append(out, Change{Space: n, Kind: "removed", Details: changedFields(x, Space{}).details})
		default:
			if c := changedFields(x, y); len(c.fields) > 0 {
				out = append(out, Change{Space: n, Kind: "changed", Fields: c.fields, Details: c.details})
			}
		}
	}
	return out
}

type fieldChanges struct {
	fields, details []string
}

func changedFields(x, y Space) fieldChanges {
	var c fieldChanges
	if x.Kind != y.Kind {
		c.fields = append(c.fields, "kind")
		c.details = append(c.details, fmt.Sprintf("kind: %q -> %q", x.Kind, y.Kind))
	}
	for _, l := range []struct {
		name string
		a, b []string
	}{
		{"github_orgs", x.GitHubOrgs, y.GitHubOrgs},
		{"roots", x.Roots, y.Roots},
		{"web_prefixes", x.WebPrefixes, y.WebPrefixes},
		{"chat", x.Chat, y.Chat},
		{"reads", x.Reads, y.Reads},
	} {
		if slices.Equal(l.a, l.b) {
			continue
		}
		c.fields = append(c.fields, l.name)
		var parts []string
		for _, v := range l.a {
			if !slices.Contains(l.b, v) {
				parts = append(parts, fmt.Sprintf("-%q", v))
			}
		}
		for _, v := range l.b {
			if !slices.Contains(l.a, v) {
				parts = append(parts, fmt.Sprintf("+%q", v))
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "reordered")
		}
		c.details = append(c.details, l.name+": "+strings.Join(parts, " "))
	}
	if x.TermsFile != y.TermsFile {
		c.fields = append(c.fields, "terms_file")
		c.details = append(c.details, fmt.Sprintf("terms_file: %q -> %q", x.TermsFile, y.TermsFile))
	}
	if x.GHConfigDir != y.GHConfigDir {
		// Reason: a changed profile changes which GitHub account the space's
		// calls run as, so the reviewer must see it.
		c.fields = append(c.fields, "gh_config_dir")
		c.details = append(c.details, fmt.Sprintf("gh_config_dir: %q -> %q", x.GHConfigDir, y.GHConfigDir))
	}
	if x.ClaudeConfigDir != y.ClaudeConfigDir {
		// Reason: as with gh_config_dir, it changes which login the space's
		// launched sessions run as.
		c.fields = append(c.fields, "claude_config_dir")
		c.details = append(c.details, fmt.Sprintf("claude_config_dir: %q -> %q", x.ClaudeConfigDir, y.ClaudeConfigDir))
	}
	return c
}
