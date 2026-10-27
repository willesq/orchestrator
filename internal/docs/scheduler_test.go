package docs

import (
	"context"
	"errors"
	"testing"
)

// ghFunc adapts a function to the GitHub seam, for fakes that need to run
// side effects mid-scan.
type ghFunc func(ctx context.Context, owner string) ([]Repo, error)

func (f ghFunc) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	return f(ctx, owner)
}

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
			s := NewScheduler(reg, ing, opts, 0)

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

// TestSchedulerScanErrors covers the failure paths a scan has to survive: they
// are reported and the scan moves on, never aborting the daemon's tick.
func TestSchedulerScanErrors(t *testing.T) {
	ctx := context.Background()
	const now int64 = 1_700_000_000

	t.Run("enumeration failure flags the owner", func(t *testing.T) {
		reg := testRegistry(t)
		if err := reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600}); err != nil {
			t.Fatal(err)
		}
		ing := &fakeIng{}
		s := NewScheduler(reg, ing, SyncOpts{
			RepoCache: t.TempDir(),
			GH:        fakeGH{err: errors.New(`exec: "gh": executable file not found in $PATH`)},
			Git:       &fakeGit{},
			Now:       func() int64 { return now },
		}, 0)

		s.Scan(ctx)

		owners, err := reg.ListOwners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if owners[0].Status != "error" {
			t.Errorf("owner status = %q, want %q", owners[0].Status, "error")
		}
		if owners[0].LastSynced != now {
			t.Errorf("last_synced = %d, want %d — a failed owner must still advance so it retries on cadence", owners[0].LastSynced, now)
		}
		if len(ing.uris) != 0 {
			t.Errorf("ingested %v, want nothing when enumeration fails", ing.uris)
		}
	})

	t.Run("due-owner lookup failure returns without syncing", func(t *testing.T) {
		reg := testRegistry(t)
		if err := reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600}); err != nil {
			t.Fatal(err)
		}
		ing := &fakeIng{}
		s := NewScheduler(reg, ing, SyncOpts{
			RepoCache: t.TempDir(),
			GH:        fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}},
			Git:       &fakeGit{layout: map[string]map[string]string{"docs": {"README.md": "x"}}},
			Now:       func() int64 { return now },
		}, 0)
		if err := reg.db.Close(); err != nil {
			t.Fatal(err)
		}

		s.Scan(ctx)

		if len(ing.uris) != 0 {
			t.Errorf("ingested %v, want nothing when the registry is unreadable", ing.uris)
		}
	})

	t.Run("mark-synced failure does not abort the remaining owners", func(t *testing.T) {
		reg := testRegistry(t)
		for _, o := range []string{"acme", "beta"} {
			if err := reg.AddOwner(ctx, Owner{Owner: o, RefreshSecs: 3600}); err != nil {
				t.Fatal(err)
			}
		}
		// Dropping the table the scheduler writes back to leaves DueOwners already
		// resolved and makes every MarkSynced fail.
		var seen []string
		var dropErr error
		gh := ghFunc(func(ctx context.Context, owner string) ([]Repo, error) {
			seen = append(seen, owner)
			if len(seen) == 1 {
				_, dropErr = reg.db.ExecContext(ctx, `DROP TABLE docs_owners`)
			}
			return nil, nil
		})
		s := NewScheduler(reg, &fakeIng{}, SyncOpts{
			RepoCache: t.TempDir(),
			GH:        gh,
			Git:       &fakeGit{},
			Now:       func() int64 { return now },
		}, 0)

		s.Scan(ctx)

		if dropErr != nil {
			t.Fatalf("drop docs_owners: %v", dropErr)
		}
		if len(seen) != 2 {
			t.Errorf("enumerated %v, want both owners — the loop must continue past a write failure", seen)
		}
	})
}

func TestNewSchedulerDefaults(t *testing.T) {
	tests := []struct{ name string }{{name: "nil clock gets a default"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScheduler(nil, nil, SyncOpts{}, 0)
			if s.opts.Now == nil {
				t.Fatal("default clock not applied")
			}
			if s.opts.Now() <= 0 {
				t.Errorf("default clock returned %d, want a unix timestamp", s.opts.Now())
			}
		})
	}
}
