package sources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/gitdates"
	"github.com/willesq/orchestrator/internal/rag"
)

// pruningIngester also lists what a writer stored and deletes sources.
type pruningIngester struct {
	fakeIngester
	stored  map[string]string // uri -> writer
	deleted []string
	listErr error
}

func (p *pruningIngester) SourcesWrittenBy(_ context.Context, prefix, writer string) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	var out []string
	for u, w := range p.stored {
		if w == writer && strings.HasPrefix(u, prefix) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (p *pruningIngester) DeleteSource(_ context.Context, uri string) (int, error) {
	p.deleted = append(p.deleted, uri)
	return 1, nil
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes dir look like a checkout on branch, with origin's default.
func gitRepo(t *testing.T, dir, branch, defaultBranch string) {
	t.Helper()
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/"+branch+"\n")
	write(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n")
	if defaultBranch != "" {
		write(t, filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD"), "ref: refs/remotes/origin/"+defaultBranch+"\n")
	}
}

func TestRefreshUsesCheckoutURIsDatesAndPrunesOwnDocs(t *testing.T) {
	root := resolve(t.TempDir())
	repo := filepath.Join(root, "willesq", "proj")
	write(t, filepath.Join(repo, "README.md"), "# doc")
	write(t, filepath.Join(repo, "docs", "KEEP.md"), "# keep")
	gitRepo(t, repo, "main", "main")
	commit := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dates := func(context.Context, string) gitdates.Dates {
		return gitdates.Dates{Head: commit, Files: map[string]time.Time{"README.md": commit.Add(-time.Hour)}}
	}
	pre := CheckoutURI(repo) + "/"
	ing := &pruningIngester{stored: map[string]string{
		pre + "README.md": rag.WriterSources, pre + "docs/KEEP.md": rag.WriterSources,
		pre + "docs/GONE.md":            rag.WriterSources,
		pre + "docs/NOTE.md":            rag.WriterClient, // an agent's note under the same prefix
		CheckoutURI(repo+"x") + "/A.md": rag.WriterSources,
	}}
	for _, s := range []Source{{Kind: KindRepo, URI: repo, Owner: "willesq"}, {Kind: KindOwner, URI: "willesq"}} {
		ing.deleted, ing.uris = nil, nil
		status, err := Refresh(context.Background(), s, ing, RefreshOpts{KnownRoots: []string{root}, Dates: dates})
		if err != nil || status != "ok" {
			t.Fatalf("%s: %q, %v", s.Kind, status, err)
		}
		if !slices.Equal(ing.deleted, []string{pre + "docs/GONE.md"}) {
			t.Fatalf("%s deleted %v", s.Kind, ing.deleted)
		}
		for _, u := range ing.uris {
			if !strings.HasPrefix(u, "checkout://") {
				t.Fatalf("stored under %s, not checkout://", u)
			}
		}
	}
	if ing.last.Writer != rag.WriterSources || (!ing.last.StartedAt.Equal(commit) && !ing.last.StartedAt.Equal(commit.Add(-time.Hour))) {
		t.Fatalf("last meta = %+v", ing.last)
	}

	// A failed listing disables pruning rather than deleting blindly.
	ing.deleted, ing.listErr = nil, errors.New("down")
	_, _ = Refresh(context.Background(), Source{Kind: KindRepo, URI: repo}, ing, RefreshOpts{Dates: dates})
	if len(ing.deleted) != 0 {
		t.Fatalf("deleted %v without a listing", ing.deleted)
	}
}

func TestRefreshSkipsCheckoutsOffTheDefaultBranch(t *testing.T) {
	for _, tc := range []struct{ branch, def string }{{"feature/x", "main"}, {"", "main"}, {"pr-head", ""}} {
		repo := filepath.Join(resolve(t.TempDir()), "proj")
		write(t, filepath.Join(repo, "README.md"), "# a PR's readme")
		gitRepo(t, repo, tc.branch, tc.def)
		if tc.branch == "" { // detached HEAD
			write(t, filepath.Join(repo, ".git", "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
		}
		ing := &pruningIngester{stored: map[string]string{CheckoutURI(repo) + "/OLD.md": rag.WriterSources}}
		status, err := Refresh(context.Background(), Source{Kind: KindRepo, URI: repo}, ing, RefreshOpts{})
		if err != nil || status != "ok" || ing.calls != 0 || len(ing.deleted) != 0 {
			t.Fatalf("%+v: status %q, %d stored, deleted %v", tc, status, ing.calls, ing.deleted)
		}
	}
	// master with no recorded default counts as default.
	repo := filepath.Join(resolve(t.TempDir()), "proj")
	write(t, filepath.Join(repo, "README.md"), "# readme")
	gitRepo(t, repo, "master", "")
	ing := &pruningIngester{}
	if _, err := Refresh(context.Background(), Source{Kind: KindRepo, URI: repo}, ing, RefreshOpts{}); err != nil || ing.calls != 1 {
		t.Fatalf("master: %d stored, %v", ing.calls, err)
	}
}

func TestPruneRefusesAMassDeletion(t *testing.T) {
	repo := filepath.Join(resolve(t.TempDir()), "proj")
	write(t, filepath.Join(repo, "README.md"), "# only this is left")
	stored := map[string]string{CheckoutURI(repo) + "/README.md": rag.WriterSources}
	for i := range 10 {
		stored[fmt.Sprintf("%s/docs/%d.md", CheckoutURI(repo), i)] = rag.WriterSources
	}
	ing := &pruningIngester{stored: stored}
	_, _ = Refresh(context.Background(), Source{Kind: KindRepo, URI: repo}, ing, RefreshOpts{})
	if len(ing.deleted) != 0 {
		t.Fatalf("a walk missing 10 of 11 docs deleted %d", len(ing.deleted))
	}
}

func TestCanonical(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	root := resolve(t.TempDir())
	local := filepath.Join(root, "local", "notes")
	_ = reg.Add(ctx, Source{Kind: KindRepo, URI: local})
	_ = reg.Add(ctx, Source{Kind: KindOwner, URI: "acme"})
	dreg := docs.NewRegistry(reg.db)
	_ = dreg.AddOwner(ctx, docs.Owner{Owner: "acme", RefreshSecs: 3600})
	_ = dreg.SetRepoState(ctx, docs.RepoState{Owner: "acme", Repo: "widget", HeadSHA: "s", Status: "ok"})
	checkout := filepath.Join(root, "elsewhere", "widget")
	gitRepo(t, checkout, "main", "main")
	untracked := filepath.Join(root, "elsewhere", "other")
	gitRepo(t, untracked, "main", "main")
	write(t, filepath.Join(untracked, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/notsynced.git\n")
	canon := Canonical(reg.db, []string{root})
	cases := map[string][]string{
		filepath.Join(local, "docs", "a.md"):                    {CheckoutURI(filepath.Join(local, "docs", "a.md"))},
		filepath.Join(root, "acme", "tool", "README.md"):        {CheckoutURI(filepath.Join(root, "acme", "tool", "README.md"))},
		filepath.Join(checkout, "docs", "PLAN.md"):              {"repo://acme/widget/docs/PLAN.md"},
		filepath.Join(untracked, "README.md"):                   nil, // origin names a repo no sync tracks
		filepath.Join(root, "acme", "README.md"):                nil, // the owner dir itself is no repo
		filepath.Join(root, "unrelated", "x.md"):                nil,
		filepath.Join(root, "acme", "..", "other", "x.md"):      nil, // cleaned: not under acme
		filepath.Join(root, "acme", "repo", "..", "..", "x.md"): nil,
		"relative/x.md": nil,
	}
	for path, want := range cases {
		got := canon(ctx, path)
		if !slices.Equal(got, want) {
			t.Errorf("Canonical(%s) = %v, want %v", path, got, want)
		}
		for _, u := range got {
			if strings.Contains(u, "/../") || strings.Contains(u, "/./") {
				t.Errorf("Canonical(%s) = %s has a dot segment", path, u)
			}
		}
	}
	// ~/.claude kept in git is live configuration, not a repo copy.
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	gitRepo(t, claude, "main", "main")
	write(t, filepath.Join(claude, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n")
	if got := Canonical(reg.db, []string{"~/code"})(ctx, filepath.Join(claude, "CLAUDE.md")); got != nil {
		t.Errorf("~/.claude mapped to %v", got)
	}
	if within("/a/b", "") || !within("/a/b", "/a") || within("/ab", "/a") {
		t.Error("within")
	}
	if !hasDotSegment("a/../b") || hasDotSegment("a/..b/c") || !hasDotSegment(".") {
		t.Error("hasDotSegment")
	}
}
