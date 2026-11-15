package prs

import (
	"context"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/github"
)

// TestSweepStampsItemsWithTheirOwnersSpace is SP-026.1.a2 for the PR
// cache: each cached item carries the space its repo owner is bound to,
// whatever the sweep it was first cached in, and its events carry it so
// the hook shows them only to sessions that read that space.
func TestSweepStampsItemsWithTheirOwnersSpace(t *testing.T) {
	gh := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {
			result("https://github.com/acme/api/pull/1", "Acme/api", "willesq"),
			result("https://github.com/globex/x/pull/2", "globex/x", "willesq"),
		},
	}}
	bound := map[string]string{"acme": "acme", "globex": "globex"}
	watch := func(context.Context) (Watch, error) {
		return Watch{Login: "willesq", Scopes: []github.Scope{scope("acme", github.ItemsInvolving, github.ReposAll), scope("globex", github.ItemsInvolving, github.ReposAll)},
			Space: func(owner string) string { return bound[strings.ToLower(owner)] }}, nil
	}
	b, state := newBabysitter(t, gh, watch)
	if err := b.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached := LoadState(state).PRs
	if cached["https://github.com/acme/api/pull/1"].Space != "acme" || cached["https://github.com/globex/x/pull/2"].Space != "globex" {
		t.Fatalf("cached spaces = %+v", cached)
	}
	evs, err := events.Read(b.eventsDir, "pr-babysitter")
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	for _, e := range evs {
		if owner, _, _ := strings.Cut(strings.ToLower(e.Summary), "/"); e.Space != bound[owner] {
			t.Errorf("event %q carries space %q", e.Summary, e.Space)
		}
	}
	// A later sweep restamps under the policy then applied.
	bound["globex"] = "quarantine"
	if err := b.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(state).PRs["https://github.com/globex/x/pull/2"].Space; got != "quarantine" {
		t.Errorf("restamped space = %q", got)
	}
	// With no Space function nothing is stamped.
	b2, state2 := newBabysitter(t, gh, func(context.Context) (Watch, error) {
		return Watch{Login: "willesq", Scopes: []github.Scope{scope("acme", github.ItemsInvolving, github.ReposAll)}}, nil
	})
	if err := b2.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(state2).PRs["https://github.com/acme/api/pull/1"].Space; got != "" {
		t.Errorf("unstamped space = %q", got)
	}
}
