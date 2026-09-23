package docs

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerScan(t *testing.T) {
	tests := []struct{ name string }{{name: "syncs due owners, marks synced, respects cadence"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()
			_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600})
			git := &fakeGit{layout: map[string]map[string]string{"docs": {"README.md": "x"}}}
			ing := &fakeIng{}
			const now int64 = 1_700_000_000
			opts := SyncOpts{
				RepoCache: t.TempDir(),
				GH:        fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}},
				Git:       git,
				Now:       func() int64 { return now },
			}
			s := NewScheduler(reg, ing, opts, time.Minute)

			s.Scan(ctx)
			if len(ing.uris) != 1 {
				t.Fatalf("scheduler did not ingest due owner: %v", ing.uris)
			}
			owners, _ := reg.ListOwners(ctx)
			if owners[0].Status != "ok" || owners[0].LastSynced != now {
				t.Fatalf("owner not marked synced: %+v", owners)
			}

			// Immediate re-scan: owner synced at 1000, now still 1000 → not due.
			ing.uris = nil
			s.Scan(ctx)
			if len(ing.uris) != 0 {
				t.Fatalf("owner should not be due on immediate re-scan, ingested %v", ing.uris)
			}
		})
	}
}

func TestNewSchedulerDefaults(t *testing.T) {
	tests := []struct{ name string }{{name: "zero tick and nil clock get defaults"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScheduler(nil, nil, SyncOpts{}, 0)
			if s.tick <= 0 || s.opts.Now == nil {
				t.Fatal("defaults not applied")
			}
		})
	}
}

func TestSchedulerRunCancels(t *testing.T) {
	tests := []struct{ name string }{{name: "Run returns promptly on ctx cancel"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx, cancel := context.WithCancel(context.Background())
			s := NewScheduler(reg, &fakeIng{}, SyncOpts{GH: fakeGH{}, Git: &fakeGit{}, Now: func() int64 { return 0 }}, time.Hour)
			done := make(chan struct{})
			go func() { s.Run(ctx); close(done) }()
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return on cancel")
			}
		})
	}
}
