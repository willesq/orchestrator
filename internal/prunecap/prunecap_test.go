package prunecap

import (
	"context"
	"errors"
	"fmt"
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
	for i, c := range []struct{ stored, gone int }{{20, 0}, {20, 10}, {3, 2}, {1, 1}} {
		p := fmt.Sprintf("checkout:///r%d/", i)
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
	if _, err := g.Confirm(ctx, "other"); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm under another key = %v", err)
	}
	granted, err := g.Confirm(ctx, "k")
	if err != nil || !slices.Equal(granted, []string{p}) {
		t.Fatalf("Confirm = %v, %v", granted, err)
	}
	// Another prefix cannot use it.
	if ok, _ := decide(t, g, "k", "checkout:///s/", 20, 11); ok {
		t.Fatal("grant used by another prefix")
	}
	*now = now.Add(30 * time.Minute)
	if ok, _ := decide(t, g, "k", p, 20, 11); !ok {
		t.Fatal("granted prune refused")
	}
	// Used once; the confirmed prune reset the baseline to the 9 left.
	if ok, why := decide(t, g, "k", p, 9, 5); ok || !strings.Contains(why, "5 of 9") {
		t.Fatalf("grant used twice, or baseline not reset: %v %q", ok, why)
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

// QA r4-1: a grant is for the refusal the operator saw, never a larger one
// recorded after it.
func TestALargerRefusalDropsTheGrant(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	if err := g.Settled(ctx, p, 100); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 100, 60); ok {
		t.Fatal("60 of 100 allowed")
	}
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 100, 100); ok {
		t.Fatal("100 allowed by a grant for 60")
	}
	if ok, _ := decide(t, g, "k", p, 100, 100); ok {
		t.Fatal("the larger refusal inherited the grant")
	}
	if ok, _ := decide(t, g, "k", p, 100, 60); ok {
		t.Fatal("grant survived a larger refusal")
	}
	// Confirming again grants the refusal now shown, 60.
	if _, err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 100, 60); !ok {
		t.Fatal("re-confirmed prune refused")
	}
}

// QA r4-2: repeated half-prunes without a clean walk between them cannot walk
// a repo down; they need a confirmation once they add up to half.
func TestRepeatedPrunesCannotWalkTheCapDown(t *testing.T) {
	g, _ := guard(t)
	const p = "checkout:///r/"
	stored, refreshes := 1000, 0
	for ; refreshes < 20; refreshes++ {
		ok, _ := decide(t, g, "k", p, stored, stored/2)
		if !ok {
			break
		}
		stored -= stored / 2
	}
	if refreshes != 1 || stored != 500 {
		t.Fatalf("after %d refreshes %d remain; want one prune to 500", refreshes, stored)
	}
	// Nor with a clean walk between prunes: the walk after a prune finds
	// nothing more gone, which does not reset what was removed.
	const q = "checkout:///q/"
	stored = 1000
	for i := 0; i < 20; i++ {
		if ok, _ := decide(t, g, "k", q, stored, stored/2); !ok {
			break
		}
		stored -= stored / 2
		if err := g.Settled(context.Background(), q, stored); err != nil {
			t.Fatal(err)
		}
	}
	if stored != 500 {
		t.Fatalf("prunes with clean walks between walked 1000 down to %d", stored)
	}
	// Growing back past the baseline does reset it.
	if err := g.Settled(context.Background(), q, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", q, 1000, 500); !ok {
		t.Fatal("half of a regrown repo refused")
	}
	// Small prunes add up the same way.
	for ok := true; ok; {
		ok, _ = decide(t, g, "k", "checkout:///s/", 1000, 100)
		if ok {
			stored -= 100
		}
	}
	ok, why := decide(t, g, "k", "checkout:///s/", 1000, 1)
	if ok || !strings.Contains(why, "on top of 500 removed") {
		t.Fatalf("after five tenths: %v %q", ok, why)
	}
}

func TestBaselineRisesWithGrowthAndResetsOnACleanWalk(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	const p = "checkout:///r/"
	// r3-4: a repo that grew from 4 to 200 in the walk that also removed 40.
	if err := g.Settled(ctx, p, 4); err != nil {
		t.Fatal(err)
	}
	if ok, why := decide(t, g, "k", p, 200, 40); !ok {
		t.Fatalf("40 of a grown 200 refused: %s", why)
	}
	// r4-8: what is stored now bounds one prune too.
	if err := g.Settled(ctx, p, 100); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 10, 9); ok {
		t.Fatal("9 of the 10 stored allowed against a baseline of 100")
	}
	// A clean walk resets everything.
	if err := g.Settled(ctx, p, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Confirm(ctx, "k"); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm after a clean walk = %v", err)
	}
	if ok, _ := decide(t, g, "k", p, 10, 5); !ok {
		t.Fatal("5 of 10 refused after reset")
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

// Primary M3: a refusal the latest walk did not see again is not granted.
func TestForgetClearsRefusalsNotSeenAgain(t *testing.T) {
	g, _ := guard(t)
	ctx := context.Background()
	key := SourceKey("owner|acme")
	decide(t, g, key, "checkout:///a/", 10, 8)
	decide(t, g, key, "checkout:///gone/", 10, 9)
	decide(t, g, "other", "checkout:///x/", 10, 9)
	if _, err := g.Confirm(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := g.Forget(ctx, key, []string{"checkout:///a/"}); err != nil {
		t.Fatal(err)
	}
	granted, err := g.Confirm(ctx, key)
	if err != nil || !slices.Equal(granted, []string{"checkout:///a/"}) {
		t.Fatalf("Confirm after Forget = %v, %v", granted, err)
	}
	// Its grant went with it.
	if ok, _ := decide(t, g, key, "checkout:///gone/", 10, 9); ok {
		t.Fatal("a forgotten refusal's grant was used")
	}
	// Another key is untouched.
	if granted, _ := g.Confirm(ctx, "other"); len(granted) != 1 {
		t.Fatalf("other key = %v", granted)
	}
	if err := g.Forget(ctx, key, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Confirm(ctx, key); !errors.Is(err, ErrNothingRefused) {
		t.Fatalf("Confirm after forgetting all = %v", err)
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
	if err := g.Settled(ctx, p, 10); err != nil {
		t.Fatal(err)
	}
	if ok, _ := decide(t, g, "k", p, 10, 6); ok {
		t.Fatal("grant outlived a clean walk")
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
	if err := g.Forget(ctx, "k", nil); err == nil {
		t.Error("Forget on a closed store")
	}
	if _, err := g.Confirm(ctx, "k"); err == nil || errors.Is(err, ErrNothingRefused) {
		t.Errorf("Confirm on a closed store = %v", err)
	}
}
