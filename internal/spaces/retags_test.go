package spaces

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestRetagStore: a retag is recorded as moving, read back as the sticky
// override, replaced by a later retag of the same source, and listed as
// pending until it is marked done; marking a stale retag done leaves a newer
// one pending.
func TestRetagStore(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	e := openEngine(t, conn)
	tick := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	e.now = func() time.Time { tick = tick.Add(time.Second); return tick }

	if over, err := e.RetagOf(ctx, "manual://plan"); err != nil || over != "" {
		t.Fatalf("no retag: %q, %v", over, err)
	}
	if err := e.SaveRetag(ctx, "", "acme", nil, false, "op"); err == nil {
		t.Fatal("a retag with no source was saved")
	}
	if err := e.SaveRetag(ctx, "manual://plan", "acme", []string{"quarantine"}, true, "operator (process 42)"); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveRetag(ctx, "manual://other", "personal", nil, false, "operator (process 42)"); err != nil {
		t.Fatal(err)
	}
	if over, err := e.RetagOf(ctx, "manual://plan"); err != nil || over != "acme" {
		t.Fatalf("override = %q, %v", over, err)
	}
	pending, err := e.PendingRetags(ctx)
	if err != nil || len(pending) != 2 || pending[0].SourceURI != "manual://plan" || pending[0].State != RetagMoving ||
		!slices.Equal(pending[0].HeldIn, []string{"quarantine"}) || pending[0].By != "operator (process 42)" || !pending[0].Confirmed || pending[1].HeldIn != nil || pending[1].Confirmed {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	// A second retag replaces the first; finishing the first's move leaves it.
	if err := e.SaveRetag(ctx, "manual://plan", "globex", []string{"acme", "quarantine"}, true, "op"); err != nil {
		t.Fatal(err)
	}
	if err := e.FinishRetag(ctx, "manual://plan", "acme"); err != nil {
		t.Fatal(err)
	}
	if over, _ := e.RetagOf(ctx, "manual://plan"); over != "globex" {
		t.Fatalf("override after a second retag = %q", over)
	}
	if err := e.FinishRetag(ctx, "manual://plan", "globex"); err != nil {
		t.Fatal(err)
	}
	pending, err = e.PendingRetags(ctx)
	if err != nil || len(pending) != 1 || pending[0].SourceURI != "manual://other" {
		t.Fatalf("pending after finishing = %+v, %v", pending, err)
	}
	if over, _ := e.RetagOf(ctx, "manual://plan"); over != "globex" {
		t.Fatalf("a finished retag lost its override: %q", over)
	}

	if _, err := conn.Exec(`UPDATE spaces_retags SET at = 'yesterday' WHERE source_uri = 'manual://other'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PendingRetags(ctx); err == nil {
		t.Fatal("a damaged time was read")
	}
	_ = conn.Close()
	if _, err := e.PendingRetags(ctx); err == nil {
		t.Fatal("a closed store listed retags")
	}
	if _, err := e.RetagOf(ctx, "manual://plan"); err == nil {
		t.Fatal("a closed store read a retag")
	}
}
