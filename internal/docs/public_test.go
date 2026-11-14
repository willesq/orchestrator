package docs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// placingStore is a store that places each doc as the gate would: verified
// public in "oss", anything else in quarantine. It logs every write, delete
// and clone in order.
type placingStore struct {
	space     map[string]string
	log       *[]string
	deleteErr error
}

func (p *placingStore) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	sp := "quarantine"
	if m.Origin.Visibility == "public" {
		sp = "oss"
	}
	p.space[m.SourceURI] = sp
	*p.log = append(*p.log, "write "+m.SourceURI+" "+sp)
	return 1, nil
}

func (p *placingStore) Copy(_ context.Context, uri string) (rag.StoredCopy, bool, error) {
	sp, ok := p.space[uri]
	return rag.StoredCopy{Writer: rag.WriterDocs, Space: sp, ContentHash: rag.ContentHash("x")}, ok, nil
}

func (p *placingStore) DeleteSource(_ context.Context, uri string) (int, error) {
	*p.log = append(*p.log, "delete "+uri)
	if p.deleteErr != nil {
		return 0, p.deleteErr
	}
	delete(p.space, uri)
	return 1, nil
}

// loggingGit logs each clone into the store's log.
type loggingGit struct {
	*fakeGit
	log *[]string
}

func (g loggingGit) Sync(ctx context.Context, owner, repo, dest string) error {
	*g.log = append(*g.log, "clone "+repo)
	return g.fakeGit.Sync(ctx, owner, repo, dest)
}

// leavesOSS is the daemon's check reduced to this store: a doc in "oss"
// leaves unless it is written now as verified public.
func leavesOSS(_ context.Context, stored string, m rag.Meta) bool {
	return stored == "oss" && m.Origin.Visibility != "public" && m.Writer == rag.WriterDocs
}

// TestRepoTurnedPrivateLeavesPublicFirst is SP-030.1.a2 (and a3 for a repo
// listed without a visibility GitHub vouched for): a repo whose HEAD did not
// move but that GitHub no longer reports public has its docs taken out of
// public before any other repo is cloned or written, and is synced again
// first, into the space it is placed in now.
func TestRepoTurnedPrivateLeavesPublicFirst(t *testing.T) {
	ctx := context.Background()
	var log []string
	st := &placingStore{space: map[string]string{}, log: &log}
	git := loggingGit{&fakeGit{layout: map[string]map[string]string{
		"aaa": {"README.md": "new"}, "lib": {"README.md": "lib"}, "tool": {"README.md": "tool"}, "keep": {"README.md": "keep"},
	}}, &log}
	public := func(name, head string) Repo { return Repo{Name: name, HeadSHA: head, OwnerNodeID: "O_oss"} }
	gh := &fakeGH{repos: []Repo{public("lib", "h1"), public("tool", "t1"), public("keep", "k1")}}
	reg, _, opts := syncDeps(t, gh, git)
	opts.GH = gh
	opts.LeavesPublic = leavesOSS
	if res := SyncOwner(ctx, reg, Owner{Owner: "oss-org"}, st, opts); res.Status != "ok" || res.Synced != 3 {
		t.Fatalf("first sync = %+v", res)
	}
	if st.space["repo://oss-org/lib/README.md"] != "oss" {
		t.Fatalf("spaces = %v", st.space)
	}

	// lib turned private and tool is listed with no visibility; neither
	// HEAD moved. aaa is new and listed first.
	gh.repos = []Repo{public("aaa", "n1"), {Name: "lib", HeadSHA: "h1", Private: true, OwnerNodeID: "O_oss"},
		{Name: "tool", HeadSHA: "t1"}, public("keep", "k1")}
	log = nil
	res := SyncOwner(ctx, reg, Owner{Owner: "oss-org"}, st, opts)
	if res.Status != "ok" || res.Synced != 3 || res.Unchanged != 1 {
		t.Fatalf("second sync = %+v", res)
	}
	want := []string{
		"delete repo://oss-org/lib/README.md", "delete repo://oss-org/tool/README.md",
		"clone lib", "write repo://oss-org/lib/README.md quarantine",
		"clone tool", "write repo://oss-org/tool/README.md quarantine",
		"clone aaa", "write repo://oss-org/aaa/README.md oss",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("order = %q\nwant    %q", log, want)
	}
	if st.space["repo://oss-org/keep/README.md"] != "oss" {
		t.Fatalf("an unchanged public repo moved: %v", st.space)
	}

	// Nothing is left in public, so the next sync takes nothing out.
	log = nil
	if res := SyncOwner(ctx, reg, Owner{Owner: "oss-org"}, st, opts); res.Unchanged != 4 || len(log) != 0 {
		t.Fatalf("third sync = %+v, %q", res, log)
	}
}

// TestLeavePublicEdges: no check, a store that cannot say where a doc is,
// a delete that fails (the doc is written again and so replaced), and a
// canceled sync.
func TestLeavePublicEdges(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	repos := []Repo{{Name: "lib", HeadSHA: "h1", Private: true}}
	if err := reg.SetHash(ctx, "repo://o/lib/a.md", "x", 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetRepoState(ctx, RepoState{Owner: "o", Repo: "lib", HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	var log []string
	st := &placingStore{space: map[string]string{"repo://o/lib/a.md": "oss"}, log: &log, deleteErr: errors.New("store down")}

	if out := leavePublic(ctx, reg, st, Owner{Owner: "o"}, repos, SyncOpts{}); out != nil {
		t.Fatalf("no check = %v", out)
	}
	if out := leavePublic(ctx, reg, &fakeIng{}, Owner{Owner: "o"}, repos, SyncOpts{LeavesPublic: leavesOSS}); out != nil {
		t.Fatalf("a store without copies = %v", out)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if out := leavePublic(canceled, reg, st, Owner{Owner: "o"}, repos, SyncOpts{LeavesPublic: leavesOSS}); len(out) != 0 {
		t.Fatalf("canceled = %v", out)
	}
	out := leavePublic(ctx, reg, st, Owner{Owner: "o"}, repos, SyncOpts{LeavesPublic: leavesOSS})
	if !out["lib"] {
		t.Fatalf("out = %v", out)
	}
	if h, _ := reg.Hash(ctx, "repo://o/lib/a.md"); h != "x" {
		t.Fatal("the hash of a doc whose delete failed was forgotten, so it would not be checked again")
	}
	if sha, _ := reg.RepoSHA(ctx, "o", "lib"); sha != "" {
		t.Fatal("the repo's HEAD was kept, so it would not be synced again")
	}
	if got := firstOut([]Repo{{Name: "a"}, {Name: "lib"}}, out); got[0].Name != "lib" {
		t.Fatalf("order = %v", got)
	}
}
