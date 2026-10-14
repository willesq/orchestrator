package routing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/models"
)

// testRouter registers claude-code (opus, sonnet aliases) and codex (gpt-sol
// with efforts), both enabled.
func testRouter(t *testing.T) *Router {
	t.Helper()
	conn := testDB(t)
	reg := models.NewRegistry(conn)
	ctx := context.Background()
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "claude-code", State: models.StateEnabled}, models.StateEnabled, []models.Model{
		{Slug: "opus", Tier: models.TierFrontier, Source: "alias", State: models.StateEnabled},
		{Slug: "sonnet", Tier: models.TierBalanced, Source: "alias", State: models.StateEnabled},
		{Slug: "claude-fable-5-1", Source: "cache"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "codex", State: models.StateEnabled}, models.StateEnabled, []models.Model{
		{Slug: "gpt-sol", Source: "cache", SupportedEfforts: []string{"low", "high"}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetModelState(ctx, "codex", "gpt-sol", models.StateEnabled); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetTier(ctx, "codex", "gpt-sol", models.TierFrontier); err != nil {
		t.Fatal(err)
	}
	return &Router{Store: NewStore(conn), Models: reg, Policy: testPolicy()}
}

func onlyClaude(p string) bool { return p == "claude-code" }

func TestRecommend(t *testing.T) {
	ctx := context.Background()
	t.Run("live proposal when the table is empty", func(t *testing.T) {
		r := testRouter(t)
		rec, err := r.Recommend(ctx, "code", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Ref() != "claude-code/opus" || rec.FromTable || rec.Confidence != "low" {
			t.Fatalf("rec = %+v", rec)
		}
	})
	t.Run("table wins over the policy", func(t *testing.T) {
		r := testRouter(t)
		if err := r.Store.Set(ctx, Route{Kind: "code", Provider: "claude-code", Model: "sonnet"}, 1); err != nil {
			t.Fatal(err)
		}
		rec, err := r.Recommend(ctx, "code", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Ref() != "claude-code/sonnet" || !rec.FromTable {
			t.Fatalf("rec = %+v", rec)
		}
	})
	t.Run("skips a provider the caller cannot run", func(t *testing.T) {
		r := testRouter(t)
		if _, err := r.Store.Apply(ctx, []Route{
			{Kind: "code", Rank: 1, Provider: "codex", Model: "gpt-sol", Effort: "high", DecidedAt: 1},
			{Kind: "code", Rank: 2, Provider: "claude-code", Model: "opus", DecidedAt: 1, EvidenceN: 7},
		}, 1); err != nil {
			t.Fatal(err)
		}
		rec, err := r.Recommend(ctx, "code", nil)
		if err != nil || rec.Ref() != "codex/gpt-sol@high" {
			t.Fatalf("unrestricted rec = %+v, %v", rec, err)
		}
		rec, err = r.Recommend(ctx, "code", onlyClaude)
		if err != nil || rec.Ref() != "claude-code/opus" || rec.Rank != 2 || rec.Confidence != "medium" {
			t.Fatalf("restricted rec = %+v, %v", rec, err)
		}
	})
	t.Run("skips a route that stopped being routable", func(t *testing.T) {
		r := testRouter(t)
		if _, err := r.Store.Apply(ctx, []Route{
			{Kind: "code", Rank: 1, Provider: "claude-code", Model: "opus", DecidedAt: 1},
			{Kind: "code", Rank: 2, Provider: "claude-code", Model: "sonnet", DecidedAt: 1},
		}, 1); err != nil {
			t.Fatal(err)
		}
		if err := r.Models.SetModelState(ctx, "claude-code", "opus", models.StateDisabled); err != nil {
			t.Fatal(err)
		}
		rec, err := r.Recommend(ctx, "code", nil)
		if err != nil || rec.Model != "sonnet" {
			t.Fatalf("rec = %+v, %v", rec, err)
		}
		if err := r.Models.SetModelState(ctx, "claude-code", "sonnet", models.StateDisabled); err != nil {
			t.Fatal(err)
		}
		// Table exhausted: the live proposal still finds codex.
		rec, err = r.Recommend(ctx, "code", nil)
		if err != nil || rec.Provider != "codex" || rec.FromTable {
			t.Fatalf("fallback rec = %+v, %v", rec, err)
		}
	})
	t.Run("a disabled provider is never recommended", func(t *testing.T) {
		r := testRouter(t)
		if err := r.Models.SetProviderState(ctx, "claude-code", models.StateDisabled); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Recommend(ctx, "code", onlyClaude); !errors.Is(err, ErrNoRoute) {
			t.Fatalf("err = %v, want ErrNoRoute", err)
		}
	})
	t.Run("drops an effort the model no longer supports", func(t *testing.T) {
		r := testRouter(t)
		if err := r.Store.Set(ctx, Route{Kind: "code", Provider: "codex", Model: "gpt-sol", Effort: "ultra"}, 1); err != nil {
			t.Fatal(err)
		}
		rec, err := r.Recommend(ctx, "code", nil)
		if err != nil || rec.Effort != "" {
			t.Fatalf("rec = %+v, %v", rec, err)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		if _, err := testRouter(t).Recommend(ctx, "poetry", nil); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestPin(t *testing.T) {
	ctx := context.Background()
	r := testRouter(t)
	tests := []struct {
		ref      string
		runnable func(string) bool
		wantErr  string
	}{
		{ref: "claude-code/opus", runnable: onlyClaude},
		{ref: "codex/gpt-sol"},
		{ref: "codex/gpt-sol", runnable: onlyClaude, wantErr: "cannot run codex"},
		{ref: "claude-code/claude-fable-5-1", wantErr: "not routable"}, // discovered, not enabled
		{ref: "claude-code/nope", wantErr: "not routable"},
		{ref: "opus", wantErr: "<provider>/<slug>"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			rec, err := r.Pin(ctx, tt.ref, tt.runnable)
			if tt.wantErr == "" {
				if err != nil || rec.Provider+"/"+rec.Model != tt.ref {
					t.Fatalf("rec = %+v, %v", rec, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfidence(t *testing.T) {
	for n, want := range map[int]string{0: "low", 4: "low", 5: "medium", 19: "medium", 20: "high"} {
		if got := confidence(n); got != want {
			t.Errorf("confidence(%d) = %s", n, got)
		}
	}
}

func TestProviderPolicyAppliesToTableAndPins(t *testing.T) {
	ctx := context.Background()
	r := testRouter(t)
	if err := r.Store.Set(ctx, Route{Kind: "code", Provider: "codex", Model: "gpt-sol"}, 1); err != nil {
		t.Fatal(err)
	}
	r.Policy.Providers = []string{"claude-code"}
	rec, err := r.Recommend(ctx, "code", nil)
	if err != nil || rec.Provider != "claude-code" || rec.FromTable {
		t.Fatalf("rec = %+v, %v; a provider dropped from the policy must not be recommended", rec, err)
	}
	if _, err := r.Pin(ctx, "codex/gpt-sol", nil); err == nil || !strings.Contains(err.Error(), "routing.providers") {
		t.Fatalf("pin err = %v", err)
	}
}
