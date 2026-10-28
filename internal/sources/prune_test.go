package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/gitdates"
	"github.com/willesq/orchestrator/internal/rag"
)

// pruningIngester also lists and deletes stored sources.
type pruningIngester struct {
	fakeIngester
	stored  []string
	deleted []string
	listErr error
}

func (p *pruningIngester) ListSources(_ context.Context, opts rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error) {
	if p.listErr != nil {
		return nil, 0, 0, p.listErr
	}
	var out []rag.SourceSummary
	for _, u := range p.stored {
		if len(u) >= len(opts.URIPrefix) && u[:len(opts.URIPrefix)] == opts.URIPrefix {
			out = append(out, rag.SourceSummary{SourceURI: u})
		}
	}
	return out, len(out), len(out), nil
}

func (p *pruningIngester) DeleteSource(_ context.Context, uri string) (int, error) {
	p.deleted = append(p.deleted, uri)
	return 1, nil
}

func TestRefreshDatesDocsAndPrunesRemovedOnes(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "willesq", "proj")
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"README.md", "docs/KEEP.md"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("# doc"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dates := func(_ context.Context, dir string) gitdates.Dates {
		if dir != repo {
			t.Errorf("dates read for %s", dir)
		}
		return gitdates.Dates{Head: commit, Files: map[string]time.Time{"README.md": commit.Add(-time.Hour)}}
	}
	pre := "repo://willesq/proj/"
	ing := &pruningIngester{stored: []string{pre + "README.md", pre + "docs/KEEP.md", pre + "docs/GONE.md", pre + "docs/SYNCED.md",
		"repo://willesq/projx/OTHER.md", "repo://other/proj/X.md"}}
	owned := func(_ context.Context, uri string) bool { return uri == pre+"docs/SYNCED.md" }
	for _, s := range []Source{{Kind: KindRepo, URI: repo, Owner: "willesq"}, {Kind: KindOwner, URI: "willesq"}} {
		ing.deleted = nil
		status, err := Refresh(context.Background(), s, ing, RefreshOpts{KnownRoots: []string{root}, Dates: dates, Owned: owned})
		if err != nil || status != "ok" {
			t.Fatalf("%s: %q, %v", s.Kind, status, err)
		}
		// Only the doc gone from this repo goes; another syncer's doc and other
		// repos' docs stay.
		if !slices.Equal(ing.deleted, []string{pre + "docs/GONE.md"}) {
			t.Fatalf("%s deleted %v", s.Kind, ing.deleted)
		}
	}
	if !ing.last.StartedAt.Equal(commit) && !ing.last.StartedAt.Equal(commit.Add(-time.Hour)) {
		t.Fatalf("doc dated %v", ing.last.StartedAt)
	}

	// A failed listing disables pruning rather than deleting blindly.
	ing.deleted, ing.listErr = nil, errors.New("down")
	if _, err := Refresh(context.Background(), Source{Kind: KindRepo, URI: repo, Owner: "willesq"}, ing, RefreshOpts{Dates: dates}); err != nil {
		t.Fatal(err)
	}
	if len(ing.deleted) != 0 {
		t.Fatalf("deleted %v without a listing", ing.deleted)
	}
}

func TestCanonical(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	root := t.TempDir()
	local := filepath.Join(root, "local", "notes")
	_ = reg.Add(ctx, Source{Kind: KindRepo, URI: local})
	_ = reg.Add(ctx, Source{Kind: KindOwner, URI: "acme"})
	checkout := filepath.Join(root, "elsewhere", "widget")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	canon := Canonical(reg.db, []string{root})
	cases := map[string][]string{
		filepath.Join(local, "docs", "a.md"):             {"repo://local/notes/docs/a.md"},
		filepath.Join(root, "acme", "tool", "README.md"): {"repo://acme/tool/README.md"},
		filepath.Join(checkout, "docs", "PLAN.md"):       {"repo://acme/widget/docs/PLAN.md"},
		filepath.Join(root, "acme", "README.md"):         nil, // the owner dir itself is no repo
		filepath.Join(root, "unrelated", "x.md"):         nil,
	}
	for path, want := range cases {
		if got := canon(ctx, path); !slices.Equal(got, want) {
			t.Errorf("Canonical(%s) = %v, want %v", path, got, want)
		}
	}
	// ~/.claude kept in git is live configuration, not a repo copy.
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claude, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(claude, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:me/dotclaude.git\n"), 0o644)
	if got := Canonical(nil, []string{"~/code"})(ctx, filepath.Join(claude, "CLAUDE.md")); got != nil {
		t.Errorf("~/.claude mapped to %v", got)
	}
	if within("/a/b", "") || !within("/a/b", "/a") || within("/ab", "/a") {
		t.Error("within")
	}
}

func TestSharedPrefixesBlockPruning(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"willesq/proj", "willesq/solo", "a/proj", "b/proj"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcs := []Source{
		{Kind: KindRepo, URI: filepath.Join(root, "a", "proj")},                   // repo://local/proj/
		{Kind: KindRepo, URI: filepath.Join(root, "b", "proj")},                   // repo://local/proj/ again
		{Kind: KindRepo, URI: filepath.Join(root, "x", "proj"), Owner: "willesq"}, // repo://willesq/proj/
		{Kind: KindOwner, URI: "willesq"},                                         // repo://willesq/{proj,solo}/
		{Kind: KindWeb, URI: "https://x"},
	}
	got := sharedPrefixes(srcs, []string{root})
	if !got["repo://local/proj/"] || !got["repo://willesq/proj/"] || got["repo://willesq/solo/"] || len(got) != 2 {
		t.Fatalf("shared = %v", got)
	}

	// A refresh through the scheduler leaves a shared prefix unpruned.
	reg := newRegistry(t)
	ctx := context.Background()
	repo := filepath.Join(root, "a", "proj")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("# a"), 0o644)
	for _, s := range srcs[:2] {
		_ = reg.Add(ctx, s)
	}
	ing := &pruningIngester{stored: []string{"repo://local/proj/README.md", "repo://local/proj/B-ONLY.md"}}
	NewScheduler(reg, ing, RefreshOpts{}).Scan(ctx)
	if len(ing.deleted) != 0 {
		t.Fatalf("a shared prefix was pruned: %v", ing.deleted)
	}
}
