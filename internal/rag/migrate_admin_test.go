package rag

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMigrationProgress(t *testing.T) {
	emb := &hookEmbedder{trigger: "sync_meta"}
	svc, _ := newMigrationService(t, emb)
	var seen MigrationProgress
	var active bool
	emb.onEmbed = func() { seen, active = svc.MigrationProgress() }
	saved := progressEvery
	progressEvery = 0 // log every step, so the log line is exercised
	t.Cleanup(func() { progressEvery = saved })

	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	if !active || seen.Target != "orchestrator-hybrid" || seen.Total != 3 || seen.Done >= seen.Total || seen.Started.IsZero() {
		t.Fatalf("progress mid-run = %+v (active %v)", seen, active)
	}
	if p, ok := svc.MigrationProgress(); ok {
		t.Fatalf("progress after the run = %+v", p)
	}
}

func TestDropCollection(t *testing.T) {
	ctx := context.Background()
	svc, f := newMigrationService(t, &hookEmbedder{})
	if _, err := svc.MigrateHybrid(ctx, "", true, nil); err != nil {
		t.Fatal(err)
	}
	backup := len(f.collection("orchestrator").points)

	rep, err := svc.DropCollection(ctx, "orchestrator", false)
	if err != nil || rep.Points != backup || rep.Dropped || f.collection("orchestrator") == nil {
		t.Fatalf("plan = %+v, %v", rep, err)
	}
	rep, err = svc.DropCollection(ctx, "orchestrator", true)
	if err != nil || !rep.Dropped || f.collection("orchestrator") != nil {
		t.Fatalf("apply = %+v, %v", rep, err)
	}
	if f.collection("orchestrator-hybrid") == nil {
		t.Fatal("the active collection was dropped")
	}
}

func TestDropCollectionRefusals(t *testing.T) {
	ctx := context.Background()
	svc, f := newMigrationService(t, &hookEmbedder{})
	f.seed("spare", map[string]any{"source_uri": "x", "chunk_index": 0, "text": "x"})
	for name, want := range map[string]string{
		"orchestrator": "using",          // the active collection
		"bad/name":     "must be",        // not a collection name
		"absent":       "does not exist", // nothing to drop
	} {
		if _, err := svc.DropCollection(ctx, name, true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("drop %q: err = %v, want %q", name, err, want)
		}
	}
	// A collection a migration is still copying into, or shadowing, stays.
	if err := os.WriteFile(svc.markerPath("spare"), []byte("orchestrator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DropCollection(ctx, "spare", true); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("marker err = %v", err)
	}
	_ = os.Remove(svc.markerPath("spare"))
	svc.setShadow(svc.active().WithCollection("spare"))
	if _, err := svc.DropCollection(ctx, "spare", true); err == nil || !strings.Contains(err.Error(), "using") {
		t.Fatalf("shadow err = %v", err)
	}
	svc.setShadow(nil)
	if f.collection("spare") == nil {
		t.Fatal("a refused drop deleted the collection")
	}
	if rep, err := svc.DropCollection(ctx, "spare", false); err != nil || rep.Points != 1 {
		t.Fatalf("plan after the refusals: %+v, %v", rep, err)
	}
	if _, err := svc.DropCollection(ctx, "absent", true); !errors.Is(err, ErrNoCollection) {
		t.Fatalf("absent err = %v", err)
	}
	// Without a state directory an unfinished migration cannot be ruled out.
	svc.StateDir = ""
	if _, err := svc.DropCollection(ctx, "spare", true); err == nil || f.collection("spare") == nil {
		t.Fatalf("no state dir: err = %v", err)
	}
}

func TestProgressTrackerLogsOnSchedule(t *testing.T) {
	svc := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 40, 4)
	tr := svc.trackProgress("t", 2)
	first := tr.logged
	tr.step(5)
	if tr.logged != first {
		t.Fatal("logged before progressEvery elapsed")
	}
	tr.logged = time.Now().Add(-progressEvery)
	tr.step(9)
	if p, _ := svc.MigrationProgress(); p.Done != 2 || p.Chunks != 9 || !tr.logged.After(first) {
		t.Fatalf("progress = %+v", p)
	}
	tr.finish()
	if _, ok := svc.MigrationProgress(); ok {
		t.Fatal("progress survived finish")
	}
}
