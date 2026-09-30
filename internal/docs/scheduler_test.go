package docs

import (
	"context"
	"testing"
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
			s := NewScheduler(reg, ing, opts)

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
	tests := []struct{ name string }{{name: "nil clock gets a default"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScheduler(nil, nil, SyncOpts{})
			if s.opts.Now == nil {
				t.Fatal("default clock not applied")
			}
			if s.opts.Now() <= 0 {
				t.Errorf("default clock returned %d, want a unix timestamp", s.opts.Now())
			}
		})
	}
}
