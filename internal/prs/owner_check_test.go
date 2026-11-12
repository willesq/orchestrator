package prs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/github"
)

// TestSweepRechecksOwnersBeforeCaching is SP-023.1.a4 for PR monitoring:
// each owner's org binding is checked once a sweep, and items of an owner
// it refuses are neither enriched nor cached.
func TestSweepRechecksOwnersBeforeCaching(t *testing.T) {
	gh := &fakeSearch{answers: map[string][]map[string]any{
		"prs --involves=@me": {
			result("https://github.com/acme/api/pull/1", "acme/api", "willesq"),
			result("https://github.com/acme/web/pull/2", "Acme/web", "willesq"),
			result("https://github.com/willesq/orch/pull/3", "willesq/orch", "willesq"),
		},
	}}
	asked := map[string]int{}
	watch := func(context.Context) (Watch, error) {
		return Watch{Login: "willesq", Scopes: []github.Scope{scope("acme", github.ItemsInvolving, github.ReposAll), scope("willesq", github.ItemsInvolving, github.ReposAll)},
			CheckOwner: func(_ context.Context, owner string) error {
				asked[strings.ToLower(owner)]++
				if strings.EqualFold(owner, "acme") {
					return errors.New("binding not confirmed")
				}
				return nil
			}}, nil
	}
	b, state := newBabysitter(t, gh, watch)
	if err := b.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := keys(LoadState(state).PRs)
	if len(got) != 1 || got[0] != "https://github.com/willesq/orch/pull/3" {
		t.Fatalf("cached %v", got)
	}
	if asked["acme"] != 1 || asked["willesq"] != 1 {
		t.Fatalf("checks = %v, want one per owner", asked)
	}
	for _, c := range gh.calls {
		if strings.Contains(c, "acme/") && strings.HasPrefix(c, "pr view") {
			t.Fatalf("a refused owner's PR was enriched: %s", c)
		}
	}
}
