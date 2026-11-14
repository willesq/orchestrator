package prs

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/willesq/orchestrator/internal/github"
)

// TestDeferredAccountKeepsItsItems (SP-074.2): an account whose hourly
// budget is spent is not searched, and its owners' items stay cached from the
// last sweep, while the default login's items are refreshed. A search that
// runs out of budget mid-sweep defers its account the same way.
func TestDeferredAccountKeepsItsItems(t *testing.T) {
	def := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {result("https://github.com/willesq/orch/pull/1", "willesq/orch", "willesq")},
	}}
	work := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {result("https://github.com/acme/api/pull/4", "acme/api", "work-me")},
	}}
	deferred := false
	route := func(owner string) (string, bool) {
		if owner == "acme" {
			return "/profiles/acme", true
		}
		return "", true
	}
	watch := func(context.Context) (Watch, error) {
		return Watch{
			Login: "willesq",
			Scopes: []github.Scope{
				scope("willesq", github.ItemsInvolving, github.ReposAll),
				scope("acme", github.ItemsInvolving, github.ReposAll),
			},
			Accounts: []Account{{Name: "/profiles/acme", Login: "work-me", GH: work.run, Deferred: deferred}},
			Route:    route,
		}, nil
	}
	b, state := newBabysitter(t, def, watch)
	ctx := context.Background()
	if err := b.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://github.com/acme/api/pull/4", "https://github.com/willesq/orch/pull/1"}
	if got := keys(LoadState(state).PRs); !slices.Equal(got, want) {
		t.Fatalf("first sweep cached %v, want %v", got, want)
	}

	// Spent before the sweep: acme's account is not searched at all.
	deferred = true
	work.calls = nil
	def.answers["prs --involves=@me"] = append(def.answers["prs --involves=@me"],
		result("https://github.com/willesq/orch/pull/2", "willesq/orch", "willesq"))
	if err := b.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(work.calls) != 0 {
		t.Errorf("a deferred account was searched: %v", work.calls)
	}
	cached := LoadState(state).PRs
	want = []string{"https://github.com/acme/api/pull/4", "https://github.com/willesq/orch/pull/1", "https://github.com/willesq/orch/pull/2"}
	if got := keys(cached); !slices.Equal(got, want) {
		t.Fatalf("deferred sweep cached %v, want %v", got, want)
	}
	if pr := cached["https://github.com/acme/api/pull/4"]; pr.CIState == "" || pr.ReviewDecision != "APPROVED" {
		t.Errorf("a kept item lost its last state: %+v", pr)
	}

	// Spent during the sweep: the search fails with ErrQuota, the other
	// account's items still refresh, and acme's are kept.
	deferred = false
	work.fail, work.failErr = "prs --involves=@me", fmt.Errorf("wrapped: %w", github.ErrQuota)
	def.answers["prs --involves=@me"] = def.answers["prs --involves=@me"][:1]
	if err := b.Sweep(ctx); err != nil {
		t.Fatalf("a spent budget failed the sweep: %v", err)
	}
	got := keys(LoadState(state).PRs)
	sort.Strings(got)
	want = []string{"https://github.com/acme/api/pull/4", "https://github.com/willesq/orch/pull/1"}
	if !slices.Equal(got, want) {
		t.Fatalf("mid-sweep deferral cached %v, want %v", got, want)
	}

	// Any other failure still fails the sweep and keeps the cache.
	work.failErr = fmt.Errorf("gh: HTTP 502")
	if err := b.Sweep(ctx); err == nil {
		t.Fatal("a failing account's search did not fail the sweep")
	}
}
