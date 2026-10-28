package prunecap

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func guard(t *testing.T) (*Guard, *time.Time) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	g := New(conn)
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now
}

func decide(t *testing.T, g *Guard, key, prefix string, stored, gone int) (bool, string) {
	t.Helper()
	ok, why, err := g.Decide(context.Background(), key, prefix, stored, gone)
	if err != nil {
		t.Fatal(err)
	}
	return ok, why
}

func TestWithinTheCapIsAllowed(t *testing.T) {
	g, _ := guard(t)
	const p = "checkout:///r/"
	for _, c := range []struct{ stored, gone int }{{20, 0}, {20, 10}, {3, 2}, {1, 1}} {
		if ok, why := decide(t, g, "k", p, c.stored, c.gone); !ok {
			t.Errorf("%d of %d refused: %s", c.gone, c.stored, why)
		}
	}
	if _, err := g.Confirm(context.Background(), "k"); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm with nothing refused = %v", err)
	}
}

func TestRefusalWaitsForAOneShotGrant(t *testing.T) {
	g, now := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	ok, why := decide(t, g, "k", p, 20, 11)
	if ok || !strings.Contains(why, "11 of 20 docs are gone") {
		t.Fatalf("over the cap: %v %q", ok, why)
	}
	// Another key does not see it.
	if _, err := g.Confirm(ctx, "other"); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm under another key = %v", err)
	}
	granted, err := g.Confirm(ctx, "k")
	if err != nil || !slices.Equal(granted, []string{p}) {
		t.Fatalf("Confirm = %v, %v", granted, err)
	}
	// A prune within the cap leaves the grant alone.
	if ok, _ := decide(t, g, "k", p, 20, 3); !ok {
		t.Fatal("within the cap refused")
	}
	// Another prefix cannot use it.
	if ok, _ := decide(t, g, "k", "checkout:///s/", 20, 11); ok {
		t.Fatal("grant used by another prefix")
	}
	*now = now.Add(30 * time.Minute)
	if ok, _ := decide(t, g, "k", p, 20, 11); !ok {
		t.Fatal("granted prune refused")
	}
	// Used once.
	if ok, _ := decide(t, g, "k", p, 20, 11); ok {
		t.Fatal("grant used twice")
	}
}

func TestGrantExpires(t *testing.T) {
	g, now := guard(t)
	const p = "checkout:///r/"
	decide(t, g, "k", p, 20, 15)
	if _, err := g.Confirm(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(GrantTTL + time.Minute)
	if ok, _ := decide(t, g, "k", p, 20, 15); ok {
		t.Fatal("expired grant used")
	}
	// A grant from the future (the clock moved back) is not used either.
	if _, err := g.Confirm(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(-time.Hour)
	if ok, _ := decide(t, g, "k", p, 20, 15); ok {
		t.Fatal("future-dated grant used")
	}
}

func TestBaselineIsTheLastCompleteWalk(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	// The baseline is what was stored after the last settled walk, not what
	// a shrinking store holds now: 20 settled, then 6 stored of which 5 gone.
	if err := g.Settled(ctx, p, 20); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 6, 5); !ok {
		t.Fatal("5 of a 20 baseline refused")
	}
	if ok, why := decide(t, g, "k", p, 20, 11); ok || !strings.Contains(why, "of 20") {
		t.Fatalf("over the cap: %v %q", ok, why)
	}
	// A confirmed prune resets the baseline to what it left.
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 20, 11); !ok {
		t.Fatal("granted prune refused")
	}
	if err := g.Settled(ctx, p, 9); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 9, 4); !ok {
		t.Fatal("4 of 9 refused after reset")
	}
	if ok, _ := decide(t, g, "k", p, 9, 5); ok {
		t.Fatal("5 of 9 allowed")
	}
	// Settling clears the refusal, so nothing waits for a grant.
	if err := g.Settled(ctx, p, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Confirm(ctx, "k"); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm after settling = %v", err)
	}
}

func TestOwnerConfirmGrantsEachRefusedRepo(t *testing.T) {
	g, _ := guard(t)
	key := DocsKey("acme")
	decide(t, g, key, "repo://acme/a/", 10, 8)
	decide(t, g, key, "repo://acme/b/", 10, 9)
	decide(t, g, key, "repo://acme/c/", 10, 1) // within the cap
	decide(t, g, DocsKey("other"), "repo://other/a/", 10, 9)
	granted, err := g.Confirm(context.Background(), key)
	if err != nil || !slices.Equal(granted, []string{"repo://acme/a/", "repo://acme/b/"}) {
		t.Fatalf("Confirm = %v, %v", granted, err)
	}
	if SourceKey("x") == DocsKey("x") {
		t.Fatal("keys collide")
	}
}

func TestStoreErrorsAreReturned(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	_ = g.db.Close()
	if _, _, err := g.Decide(ctx, "k", "p/", 20, 15); err == nil {
		t.Error("Decide on a closed store")
	}
	if err := g.Settled(ctx, "p/", 1); err == nil {
		t.Error("Settled on a closed store")
	}
	if _, err := g.Confirm(ctx, "k"); err == nil || errors.Is(err, ErrNothingRefused) {
		t.Errorf("Confirm on a closed store = %v", err)
	}
}

func TestGrantCoversOnlyTheRefusedPrune(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	decide(t, g, "k", p, 10, 6)
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	// A bigger prune than the one refused is refused again, and recorded.
	if ok, why := decide(t, g, "k", p, 10, 10); ok || !strings.Contains(why, "10 of 10") {
		t.Fatalf("bigger prune: %v %q", ok, why)
	}
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 10, 10); !ok {
		t.Fatal("re-confirmed prune refused")
	}
}

func TestSettlingDropsAnUnusedGrant(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	decide(t, g, "k", p, 10, 6)
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	// The next walk is healthy: the grant must not wait for a broken one.
	if err := g.Settled(ctx, p, 10); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 10, 6); ok {
		t.Fatal("grant outlived a healthy walk")
	}
}
