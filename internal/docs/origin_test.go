package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// metaIng records the meta of every write.
type metaIng struct{ metas []rag.Meta }

func (m *metaIng) IngestText(_ context.Context, _ string, meta rag.Meta) (int, error) {
	m.metas = append(m.metas, meta)
	return 1, nil
}

// TestRepoVisibilityAndOwnerNodeID is SP-023.1.a2: the repo listing reads
// each repo's visibility and its owner's node ID, and the docs sync reports
// both in every doc's origin.
func TestRepoVisibilityAndOwnerNodeID(t *testing.T) {
	if !strings.Contains(repoQuery, "isPrivate") || !strings.Contains(repoQuery, "owner{id}") {
		t.Fatalf("the repo query asks for neither visibility nor owner ID: %s", repoQuery)
	}
	body := `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
		{"name":"pub","isPrivate":false,"owner":{"id":"O_acme"},"defaultBranchRef":{"target":{"oid":"s1"}}},
		{"name":"priv","isPrivate":true,"owner":{"id":"O_acme"},"defaultBranchRef":{"target":{"oid":"s2"}}},
		{"name":"bare","isPrivate":false}]}}}}`
	repos, _, _, err := parseGraphQLPage([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{{Name: "pub", HeadSHA: "s1", OwnerNodeID: "O_acme"}, {Name: "priv", HeadSHA: "s2", Private: true, OwnerNodeID: "O_acme"}, {Name: "bare"}}
	for i, r := range repos {
		if r != want[i] {
			t.Fatalf("repo %d = %+v, want %+v", i, r, want[i])
		}
	}

	git := &fakeGit{layout: map[string]map[string]string{"pub": {"a.md": "a"}, "priv": {"b.md": "b"}, "bare": {"c.md": "c"}}}
	reg, _, opts := syncDeps(t, fakeGH{repos: repos}, git)
	ing := &metaIng{}
	if res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts); res.Status != "ok" {
		t.Fatalf("sync = %+v", res)
	}
	got := map[string]rag.Origin{}
	for _, m := range ing.metas {
		got[m.SourceURI] = m.Origin
	}
	for uri, o := range map[string]rag.Origin{
		"repo://acme/pub/a.md":  {System: rag.OriginGitHub, RepoOwner: "acme", RepoName: "pub", Visibility: "public", OwnerNodeID: "O_acme"},
		"repo://acme/priv/b.md": {System: rag.OriginGitHub, RepoOwner: "acme", RepoName: "priv", Visibility: "private", OwnerNodeID: "O_acme"},
		// Listed without its owner's ID: visibility unknown, never public.
		"repo://acme/bare/c.md": {System: rag.OriginGitHub, RepoOwner: "acme", RepoName: "bare"},
	} {
		if got[uri] != o {
			t.Errorf("%s origin = %+v, want %+v", uri, got[uri], o)
		}
	}
}

// TestSyncRechecksOwnerBetweenRepos is SP-023.1.a4 for docs: the owner is
// checked before listing and again before each further repo it writes;
// unchanged repos cost no check, and a refusal stops the sync before the
// next repo is cloned or written.
func TestSyncRechecksOwnerBetweenRepos(t *testing.T) {
	layout := map[string]map[string]string{"a": {"a.md": "a"}, "b": {"b.md": "b"}, "c": {"c.md": "c"}, "d": {"d.md": "d"}}
	git := &fakeGit{layout: layout}
	reg, ing, opts := syncDeps(t, fakeGH{repos: []Repo{{Name: "a", HeadSHA: "1"}, {Name: "b", HeadSHA: "1"}, {Name: "c", HeadSHA: "1"}, {Name: "d", HeadSHA: "1"}}}, git)
	ctx := context.Background()
	if err := reg.SetRepoState(ctx, RepoState{Owner: "acme", Repo: "b", HeadSHA: "1", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	checks := 0
	opts.CheckOwner = func(context.Context, string) error {
		checks++
		if checks == 3 {
			return errors.New("binding not confirmed")
		}
		return nil
	}
	res := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts)
	// Checks: before listing, (a written, b unchanged) before c, before d.
	if res.Status != "error" || res.Synced != 2 || res.Unchanged != 1 || res.NotTried != 1 || checks != 3 ||
		!strings.Contains(res.Reason, "binding not confirmed") {
		t.Fatalf("result = %+v after %d checks", res, checks)
	}
	if strings.Join(git.synced, ",") != "a,c" {
		t.Fatalf("cloned %v", git.synced)
	}
	for _, u := range ing.uris {
		if strings.Contains(u, "/d/") {
			t.Fatalf("a refused owner's repo was written: %v", ing.uris)
		}
	}
}
