package prs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/github"
)

// TestSweepSearchesEachOwnerAsItsSpacesAccount (SP-063): each owner's items
// come only through the account its space routes it to. An owner routed to a
// profile is searched and enriched as that profile; an owner whose space has
// no working profile is dropped, even when the default login's search
// returns it.
func TestSweepSearchesEachOwnerAsItsSpacesAccount(t *testing.T) {
	def := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {
			result("https://github.com/willesq/orch/pull/1", "willesq/orch", "willesq"),
			// The default login must not report acme's or globex's items.
			result("https://github.com/acme/api/pull/2", "acme/api", "willesq"),
			result("https://github.com/globex/web/pull/3", "globex/web", "willesq"),
		},
	}}
	work := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {
			result("https://github.com/acme/api/pull/4", "acme/api", "work-me"),
			result("https://github.com/willesq/orch/pull/5", "willesq/orch", "work-me"),
		},
		"prs --owner=acme": {result("https://github.com/acme/api/pull/6", "acme/api", "alice")},
	}}
	route := func(owner string) (string, bool) {
		switch strings.ToLower(owner) {
		case "acme":
			return "/profiles/acme", true
		case "globex":
			return "", false
		}
		return "", true
	}
	watch := func(context.Context) (Watch, error) {
		return Watch{
			Login: "willesq",
			Scopes: []github.Scope{
				scope("willesq", github.ItemsInvolving, github.ReposAll),
				scope("acme", github.ItemsAll, github.ReposAll),
				scope("globex", github.ItemsAll, github.ReposAll),
			},
			Accounts: []Account{{Name: "/profiles/acme", Login: "work-me", GH: work.run}},
			Route:    route,
		}, nil
	}
	b, _ := newBabysitter(t, def, watch)
	got, err := b.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, pr := range got {
		urls = append(urls, pr.URL)
		if pr.URL == "https://github.com/acme/api/pull/4" && !pr.Authored() {
			t.Errorf("a profile's own PR was not marked authored: %+v", pr)
		}
	}
	want := []string{
		"https://github.com/willesq/orch/pull/1",
		"https://github.com/acme/api/pull/4",
		"https://github.com/acme/api/pull/6",
	}
	if !slices.Equal(urls, want) {
		t.Errorf("items = %v, want %v", urls, want)
	}
	for _, c := range def.calls {
		if strings.Contains(c, "acme") || strings.Contains(c, "globex") {
			t.Errorf("the default login made a call about another space: %q", c)
		}
	}
	if !slices.Contains(work.calls, "pr view https://github.com/acme/api/pull/4 --json statusCheckRollup,reviewDecision,mergeable") {
		t.Errorf("the profile's PR was not enriched as the profile: %v", work.calls)
	}
	for _, c := range work.calls {
		if strings.Contains(c, "--owner=globex") || strings.Contains(c, "willesq/orch") {
			t.Errorf("the profile made a call about another space: %q", c)
		}
	}
}

// TestProfileOwnersNeedAnEnabledScope: the PR cache is readable by tasks in
// any space, so a profile's items are kept only for a scope the user enabled,
// as the default login's are.
func TestProfileOwnersNeedAnEnabledScope(t *testing.T) {
	def := &fakeSearch{}
	work := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {
			result("https://github.com/acme/api/pull/2", "acme/api", "work-me"),
			result("https://github.com/acme-off/api/pull/3", "acme-off/api", "work-me"),
			result("https://github.com/acme-new/api/pull/4", "acme-new/api", "work-me"),
		},
	}}
	watch := func(context.Context) (Watch, error) {
		return Watch{
			Login: "willesq",
			Scopes: []github.Scope{
				scope("acme", github.ItemsInvolving, github.ReposAll),
				{Owner: "acme-off", Enabled: false, Items: github.ItemsInvolving, RepoMode: github.ReposAll},
			},
			Accounts: []Account{{Name: "/profiles/acme", Login: "work-me", GH: work.run}},
			Route: func(owner string) (string, bool) {
				if strings.HasPrefix(owner, "acme") {
					return "/profiles/acme", true
				}
				return "", true
			},
		}, nil
	}
	b, _ := newBabysitter(t, def, watch)
	got, err := b.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://github.com/acme/api/pull/2" {
		t.Errorf("items = %+v", got)
	}
}
