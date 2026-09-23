package docs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewRegistry(conn)
}

func TestOwnerLifecycle(t *testing.T) {
	tests := []struct{ name string }{{name: "add, list, due, mark-synced, mark-due, remove"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()

			if err := reg.AddOwner(ctx, Owner{Owner: "willesq", RefreshSecs: 3600, Tags: []string{"t1"}}); err != nil {
				t.Fatal(err)
			}
			// idempotent upsert updates cadence.
			if err := reg.AddOwner(ctx, Owner{Owner: "willesq", RefreshSecs: 7200}); err != nil {
				t.Fatal(err)
			}
			owners, err := reg.ListOwners(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(owners) != 1 || owners[0].RefreshSecs != 7200 {
				t.Fatalf("want 1 owner refresh=7200, got %+v", owners)
			}

			// Realistic wall-clock: last_synced=0 with now >> refresh_secs → due.
			const now int64 = 1_700_000_000
			if due, _ := reg.DueOwners(ctx, now); len(due) != 1 {
				t.Fatalf("want 1 due, got %d", len(due))
			}
			if err := reg.MarkSynced(ctx, "willesq", now, "ok"); err != nil {
				t.Fatal(err)
			}
			if due, _ := reg.DueOwners(ctx, now+7199); len(due) != 0 {
				t.Fatalf("should not be due before interval elapsed")
			}
			if due, _ := reg.DueOwners(ctx, now+7200); len(due) != 1 {
				t.Fatalf("should be due once interval elapsed (sleep-safe catch-up)")
			}
			// mark due resets to run on next scan.
			if err := reg.MarkDue(ctx, "willesq"); err != nil {
				t.Fatal(err)
			}
			if due, _ := reg.DueOwners(ctx, now); len(due) != 1 {
				t.Fatalf("mark-due should force due")
			}
			// mark all due.
			_ = reg.MarkSynced(ctx, "willesq", now, "ok")
			if err := reg.MarkDue(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if due, _ := reg.DueOwners(ctx, now); len(due) != 1 {
				t.Fatalf("mark-all-due should force due")
			}

			if err := reg.RemoveOwner(ctx, "willesq"); err != nil {
				t.Fatal(err)
			}
			if owners, _ := reg.ListOwners(ctx); len(owners) != 0 {
				t.Fatalf("owner not removed")
			}
		})
	}
}

func TestRepoAndHashState(t *testing.T) {
	tests := []struct{ name string }{{name: "repo sha, file hash, clear caches (owner + all)"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()

			if sha, _ := reg.RepoSHA(ctx, "o", "r"); sha != "" {
				t.Fatalf("unknown repo sha should be empty, got %q", sha)
			}
			if err := reg.SetRepoState(ctx, RepoState{Owner: "o", Repo: "r", HeadSHA: "abc", Files: 3, Status: "ok"}); err != nil {
				t.Fatal(err)
			}
			if sha, _ := reg.RepoSHA(ctx, "o", "r"); sha != "abc" {
				t.Fatalf("want abc, got %q", sha)
			}
			// upsert updates.
			_ = reg.SetRepoState(ctx, RepoState{Owner: "o", Repo: "r", HeadSHA: "def", Files: 5, Status: "ok"})
			repos, _ := reg.ListRepos(ctx, "o")
			if len(repos) != 1 || repos[0].HeadSHA != "def" || repos[0].Files != 5 {
				t.Fatalf("repo state upsert wrong: %+v", repos)
			}

			uri := "repo://o/r/README.md"
			if h, _ := reg.Hash(ctx, uri); h != "" {
				t.Fatalf("unknown hash should be empty")
			}
			if err := reg.SetHash(ctx, uri, "h1", 5); err != nil {
				t.Fatal(err)
			}
			_ = reg.SetHash(ctx, uri, "h2", 6) // update
			if h, _ := reg.Hash(ctx, uri); h != "h2" {
				t.Fatalf("want h2, got %q", h)
			}

			// ClearCaches(owner) drops that owner's repo + hash state only.
			_ = reg.SetHash(ctx, "repo://x/y/z.md", "keep", 1)
			if err := reg.ClearCaches(ctx, "o"); err != nil {
				t.Fatal(err)
			}
			if sha, _ := reg.RepoSHA(ctx, "o", "r"); sha != "" {
				t.Fatalf("owner repo sha not cleared")
			}
			if h, _ := reg.Hash(ctx, uri); h != "" {
				t.Fatalf("owner hash not cleared")
			}
			if h, _ := reg.Hash(ctx, "repo://x/y/z.md"); h != "keep" {
				t.Fatalf("other owner hash should survive, got %q", h)
			}

			// ClearCaches("") drops everything.
			if err := reg.ClearCaches(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if h, _ := reg.Hash(ctx, "repo://x/y/z.md"); h != "" {
				t.Fatalf("clear-all did not drop hash")
			}
		})
	}
}

func TestRemoveOwnerClearsCaches(t *testing.T) {
	tests := []struct{ name string }{{name: "remove owner also drops its repo + hash rows"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()
			_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600})
			_ = reg.SetRepoState(ctx, RepoState{Owner: "acme", Repo: "docs", HeadSHA: "s"})
			_ = reg.SetHash(ctx, "repo://acme/docs/README.md", "h", 1)
			if err := reg.RemoveOwner(ctx, "acme"); err != nil {
				t.Fatal(err)
			}
			if repos, _ := reg.ListRepos(ctx, "acme"); len(repos) != 0 {
				t.Fatalf("repo rows not removed")
			}
			if h, _ := reg.Hash(ctx, "repo://acme/docs/README.md"); h != "" {
				t.Fatalf("hash rows not removed")
			}
		})
	}
}
