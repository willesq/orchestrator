package prunecap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func guard(t *testing.T) *Guard {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return New(conn)
}

func TestCapCountsAcrossScans(t *testing.T) {
	g := guard(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	const p = "checkout:///r/"
	// 20 stored: up to 10 may go in the window, however they are split.
	if ok, _, err := g.Allow(ctx, "k", p, 20, 6); !ok || err != nil {
		t.Fatal("6 of 20 refused")
	}
	_ = g.Done(ctx, p, 6)
	ok, why, _ := g.Allow(ctx, "k", p, 14, 5) // 11 > 10, measured against 20, not 14
	if ok || !strings.Contains(why, "6 removed since 2026-10-28, of 20 stored then") {
		t.Fatalf("walked down: %v %q", ok, why)
	}
	if ok, _, _ := g.Allow(ctx, "k", p, 14, 4); !ok {
		t.Fatal("within the budget refused")
	}
	if ok, _, _ := g.Allow(ctx, "k", p, 14, 0); !ok {
		t.Fatal("nothing gone refused")
	}
	// An operator's confirmation lets one prune through and restarts the window.
	if err := g.Confirm(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := g.Allow(ctx, "k", p, 14, 13); !ok {
		t.Fatal("confirmed prune refused")
	}
	if ok, _, _ := g.Allow(ctx, "k", p, 1, 2); !ok {
		t.Fatal("small repos may always lose two")
	}
	if ok, _, _ := g.Allow(ctx, "k", p, 1, 3); ok {
		t.Fatal("the confirmation was not consumed")
	}
	// After a window a new baseline is taken.
	now = now.Add(Window + time.Hour)
	if ok, _, _ := g.Allow(ctx, "k", p, 10, 5); !ok {
		t.Fatal("new window refused half")
	}
	if g.Done(ctx, p, 0) != nil || SourceKey("x") != "source|x" || DocsKey("o") != "docs|o" {
		t.Fatal("keys")
	}
}
