package docs

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/gitdates"
	"github.com/willesq/orchestrator/internal/rag"
)

// removingIng records ingests, with their dates, and deletions.
type removingIng struct {
	metas   []rag.Meta
	deleted []string
}

func (r *removingIng) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	r.metas = append(r.metas, m)
	return 1, nil
}

func (r *removingIng) DeleteSource(_ context.Context, uri string) (int, error) {
	r.deleted = append(r.deleted, uri)
	return 1, nil
}

// freshGit clones into an emptied destination, as the real clone does.
type freshGit struct{ *fakeGit }

func (f freshGit) Sync(ctx context.Context, owner, repo, dest string) error {
	_ = os.RemoveAll(dest)
	return f.fakeGit.Sync(ctx, owner, repo, dest)
}

func TestSyncDatesDocsAndRemovesDeletedOnes(t *testing.T) {
	git := &fakeGit{layout: map[string]map[string]string{"docs": {"README.md": "v1", "OLD.md": "old", "under_score/a.md": "a"}}}
	reg, _, opts := syncDeps(t, fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}}, freshGit{git})
	commit := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	opts.Dates = func(context.Context, string) gitdates.Dates {
		return gitdates.Dates{Head: commit, Files: map[string]time.Time{"README.md": commit.Add(-48 * time.Hour)}}
	}
	ing := &removingIng{}
	ctx := context.Background()
	o := Owner{Owner: "acme"}
	if res := SyncOwner(ctx, reg, o, ing, opts); res.Status != "ok" {
		t.Fatalf("first sync = %+v", res)
	}
	for _, m := range ing.metas {
		want := commit
		if m.SourceURI == "repo://acme/docs/README.md" {
			want = commit.Add(-48 * time.Hour)
		}
		if !m.StartedAt.Equal(want) || !m.EndedAt.Equal(want) {
			t.Errorf("%s dated %v..%v, want %v", m.SourceURI, m.StartedAt, m.EndedAt, want)
		}
	}
	// A repo with a similar name must not be pruned by this one's sync.
	_ = reg.SetHash(ctx, "repo://acme/docs2/X.md", "h", 1)

	// OLD.md leaves the repo: the next sync deletes its chunks and hash.
	git.layout["docs"] = map[string]string{"README.md": "v2", "under_score/a.md": "a"}
	opts.GH = fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s2"}}}
	if res := SyncOwner(ctx, reg, o, ing, opts); res.Status != "ok" {
		t.Fatalf("second sync = %+v", res)
	}
	if !slices.Equal(ing.deleted, []string{"repo://acme/docs/OLD.md"}) {
		t.Fatalf("deleted %v", ing.deleted)
	}
	if h, _ := reg.Hash(ctx, "repo://acme/docs/OLD.md"); h != "" {
		t.Fatal("removed doc's hash kept")
	}
	if uris, _ := reg.HashURIs(ctx, "repo://acme/docs2/"); len(uris) != 1 {
		t.Fatalf("a sibling repo's hash was touched: %v", uris)
	}

	// A clone with no markdown at all deletes nothing.
	git.layout["docs"] = map[string]string{"code.go": "x"}
	opts.GH = fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s3"}}}
	ing.deleted = nil
	SyncOwner(ctx, reg, o, ing, opts)
	if len(ing.deleted) != 0 {
		t.Fatalf("an empty clone deleted %v", ing.deleted)
	}
	if err := reg.DeleteHash(ctx, "repo://none"); err != nil {
		t.Fatal(err)
	}
}

// copyingIng also answers Copy, with each URI's stored writer and hash.
type copyingIng struct {
	removingIng
	copies map[string]rag.StoredCopy
}

func (c *copyingIng) IngestText(ctx context.Context, text string, m rag.Meta) (int, error) {
	if c.copies == nil {
		c.copies = map[string]rag.StoredCopy{}
	}
	c.copies[m.SourceURI] = rag.StoredCopy{Writer: m.Writer, ContentHash: rag.ContentHash(text)}
	return c.removingIng.IngestText(ctx, text, m)
}

func (c *copyingIng) Copy(_ context.Context, uri string) (rag.StoredCopy, bool, error) {
	cp, ok := c.copies[uri]
	return cp, ok, nil
}

func TestSyncRestoresADocAClientOverwrote(t *testing.T) {
	git := &fakeGit{layout: map[string]map[string]string{"docs": {"README.md": "the real readme"}}}
	reg, _, opts := syncDeps(t, fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}}, freshGit{git})
	ing := &copyingIng{}
	ctx := context.Background()
	o := Owner{Owner: "acme"}
	SyncOwner(ctx, reg, o, ing, opts)
	if len(ing.metas) != 1 {
		t.Fatalf("first sync stored %d", len(ing.metas))
	}
	// Unchanged repo and doc: nothing is stored again.
	SyncOwner(ctx, reg, o, ing, opts)
	if len(ing.metas) != 1 {
		t.Fatalf("an unchanged repo was re-stored: %d", len(ing.metas))
	}
	// A client overwrites the doc's URI: the next sync restores it, though
	// neither the repo's HEAD nor the file changed.
	ing.copies["repo://acme/docs/README.md"] = rag.StoredCopy{Writer: rag.WriterClient, ContentHash: "evil"}
	SyncOwner(ctx, reg, o, ing, opts)
	if len(ing.metas) != 2 || ing.copies["repo://acme/docs/README.md"].Writer != rag.WriterDocs {
		t.Fatalf("overwrite not restored: %d stores, %+v", len(ing.metas), ing.copies["repo://acme/docs/README.md"])
	}
	// Docs stored before writers were recorded do not force a re-sync.
	ing.copies["repo://acme/docs/README.md"] = rag.StoredCopy{}
	if overwritten(ctx, reg, ing, "acme", "docs") {
		t.Fatal("a legacy copy counted as overwritten")
	}
	if !storedAsSynced(ctx, &removingIng{}, "u", "t") {
		t.Fatal("without Copy the registry is trusted")
	}
}

// refusingGuard refuses every prune.
type refusingGuard struct{ keys []string }

func (g *refusingGuard) Allow(_ context.Context, key, _ string, _, _ int) (bool, string, error) {
	g.keys = append(g.keys, key)
	return false, "2 docs are gone", nil
}
func (g *refusingGuard) Done(context.Context, string, int) error { return nil }

func TestRefusedPruneShowsOnTheRepo(t *testing.T) {
	git := &fakeGit{layout: map[string]map[string]string{"docs": {"A.md": "a", "B.md": "b", "C.md": "c"}}}
	reg, _, opts := syncDeps(t, fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}}, freshGit{git})
	ing := &removingIng{}
	ctx := context.Background()
	o := Owner{Owner: "acme"}
	SyncOwner(ctx, reg, o, ing, opts)
	git.layout["docs"] = map[string]string{"A.md": "a"}
	opts.GH = fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s2"}}}
	g := &refusingGuard{}
	opts.Guard = g
	res := SyncOwner(ctx, reg, o, ing, opts)
	repos, _ := reg.ListRepos(ctx, "acme")
	if res.Status != "partial" || len(ing.deleted) != 0 || len(repos) != 1 || repos[0].Status != "error" ||
		!strings.Contains(repos[0].Error, "prune refused: 2 docs are gone") || !strings.Contains(repos[0].Error, "orch docs sync acme --confirm-prune") ||
		repos[0].HeadSHA != "s1" || g.keys[0] != "docs|acme" {
		t.Fatalf("res %+v, deleted %v, repos %+v", res, ing.deleted, repos)
	}
}
