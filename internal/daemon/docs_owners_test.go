package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// purgeRag records deletions and can fail on one URI.
type purgeRag struct {
	fakeRag
	deleted []string
	failOn  string
}

func (r *purgeRag) DeleteSource(_ context.Context, uri string) (int, error) {
	if uri == r.failOn {
		return 0, errors.New("store down")
	}
	r.deleted = append(r.deleted, uri)
	return 2, nil
}

type fakeDocsGH struct {
	repos []docs.Repo
	err   error
}

func (f fakeDocsGH) ListRepos(context.Context, string) ([]docs.Repo, error) { return f.repos, f.err }

func docsOwnerServer(t *testing.T) (*server, *purgeRag, *fakeDocsSync) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := &purgeRag{}
	sync := &fakeDocsSync{}
	return &server{rag: store, docsReg: docs.NewRegistry(conn), docsSync: sync, docsRefresh: 3600}, store, sync
}

func seedDocs(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "all"}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"keep", "drop"} {
		if err := s.docsReg.SetRepoState(ctx, docs.RepoState{Owner: "acme", Repo: repo, HeadSHA: "h", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
		if err := s.docsReg.SetHash(ctx, "repo://acme/"+repo+"/README.md", "h", 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveDocsOwner(t *testing.T) {
	ctx := context.Background()
	t.Run("validation", func(t *testing.T) {
		s, _, _ := docsOwnerServer(t)
		for _, req := range []*orchdv1.SaveDocsOwnerRequest{
			{Owner: "../etc", Enabled: true, RepoMode: "all"},
			{Owner: "", Enabled: true, RepoMode: "all"},
			{Owner: "acme", Enabled: true, RepoMode: "some"},
			{Owner: "acme", Enabled: true, RepoMode: "include"},
			{Owner: "acme", Enabled: true, RepoMode: "include", Repos: []string{"a,b"}},
		} {
			if _, err := s.SaveDocsOwner(ctx, req); grpcstatus.Code(err) != codes.InvalidArgument {
				t.Errorf("%+v: err = %v", req, err)
			}
		}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 0 {
			t.Errorf("a refused save registered %+v", out.GetOwners())
		}
	})
	t.Run("opting in registers the owner with the default cadence and starts a sync", func(t *testing.T) {
		s, _, sync := docsOwnerServer(t)
		got, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "include", Repos: []string{"a"}})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetRefreshSecs() != 3600 || !got.GetEnabled() || got.GetRepoMode() != "include" || !slices.Equal(got.GetSelectedRepos(), []string{"a"}) {
			t.Errorf("owner = %+v", got)
		}
		if sync.requests != 1 {
			t.Errorf("requests = %d", sync.requests)
		}
		out, err := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{})
		if err != nil || len(out.GetOwners()) != 1 {
			t.Fatalf("owners = %+v, %v", out.GetOwners(), err)
		}
	})
	t.Run("refused while a sync runs, saying what runs", func(t *testing.T) {
		s, _, sync := docsOwnerServer(t)
		sync.p = docs.Progress{Running: true, Owner: "other", Done: 2, Total: 9}
		_, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "all"})
		if grpcstatus.Code(err) != codes.Aborted {
			t.Fatalf("err = %v", err)
		}
		sync.p = docs.Progress{}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 0 {
			t.Errorf("refused save still registered: %+v", out.GetOwners())
		}
	})
	t.Run("another spelling of a registered owner is that owner", func(t *testing.T) {
		s, _, _ := docsOwnerServer(t)
		seedDocs(t, s)
		got, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "ACME", RepoMode: "all"})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetOwner() != "acme" || got.GetEnabled() {
			t.Errorf("owner = %+v", got)
		}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 1 {
			t.Errorf("owners = %+v", out.GetOwners())
		}
	})
	t.Run("a paused owner cannot be synced by name", func(t *testing.T) {
		s, _, _ := docsOwnerServer(t)
		if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", RepoMode: "all"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "acme"}); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPurgeDocs(t *testing.T) {
	ctx := context.Background()
	t.Run("needs confirmation", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Repos: []string{"drop"}}); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("err = %v", err)
		}
		if len(store.deleted) != 0 {
			t.Errorf("deleted %v", store.deleted)
		}
	})
	t.Run("refuses a repo still synced, since the next sync restores it", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		_, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Repos: []string{"drop"}, Confirm: true})
		if grpcstatus.Code(err) != codes.FailedPrecondition || len(store.deleted) != 0 {
			t.Errorf("err = %v, deleted %v", err, store.deleted)
		}
	})
	t.Run("deletes only what the owner stopped covering", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "exclude", Repos: []string{"drop"}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Confirm: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetPurgedSources() != 1 || got.GetPurgedChunks() != 2 || !slices.Equal(store.deleted, []string{"repo://acme/drop/README.md"}) {
			t.Errorf("got %+v, deleted %v", got, store.deleted)
		}
		repos, _ := s.docsReg.ListRepos(ctx, "acme")
		if len(repos) != 1 || repos[0].Repo != "keep" {
			t.Errorf("repos = %+v", repos)
		}
	})
	t.Run("named repos limit the delete to those repos", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", RepoMode: "all"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Repos: []string{"DROP"}, Confirm: true}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(store.deleted, []string{"repo://acme/drop/README.md"}) {
			t.Errorf("deleted %v", store.deleted)
		}
	})
	t.Run("a forced sync leaves opted-out docs purgeable", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "exclude", Repos: []string{"drop"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "acme", Force: true}); err != nil {
			t.Fatal(err)
		}
		got, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Repos: []string{"drop"}, Confirm: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetPurgedSources() != 1 || len(store.deleted) != 1 {
			t.Errorf("got %+v, deleted %v", got, store.deleted)
		}
	})
	t.Run("a failed delete keeps the hashes of what is still indexed", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		store.failOn = "repo://acme/drop/README.md"
		if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", RepoMode: "all"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Confirm: true}); err == nil {
			t.Fatal("no error")
		}
		if h, _ := s.docsReg.Hash(ctx, "repo://acme/drop/README.md"); h == "" {
			t.Error("the hash of a doc still indexed was forgotten")
		}
		if h, _ := s.docsReg.Hash(ctx, "repo://acme/keep/README.md"); h == "" && !slices.Contains(store.deleted, "repo://acme/keep/README.md") {
			t.Error("a hash went without its doc")
		}
	})
	t.Run("rejects a bad name and an unknown owner", func(t *testing.T) {
		s, _, _ := docsOwnerServer(t)
		if _, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Repos: []string{"../x"}, Confirm: true}); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("err = %v", err)
		}
		if _, err := s.PurgeDocs(ctx, &orchdv1.PurgeDocsRequest{Owner: "acme", Confirm: true}); grpcstatus.Code(err) != codes.NotFound {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRemoveDocsOwner(t *testing.T) {
	ctx := context.Background()
	t.Run("keeps what was indexed by default", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme"}); err != nil {
			t.Fatal(err)
		}
		if len(store.deleted) != 0 {
			t.Errorf("deleted %v", store.deleted)
		}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 0 {
			t.Errorf("owners = %+v", out.GetOwners())
		}
	})
	t.Run("purge needs confirmation and leaves the owner", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme", Purge: true}); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("err = %v", err)
		}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 1 || len(store.deleted) != 0 {
			t.Errorf("owners %+v, deleted %v", out.GetOwners(), store.deleted)
		}
	})
	t.Run("a failed purge leaves the owner to retry", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		store.failOn = "repo://acme/keep/README.md"
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme", Purge: true, Confirm: true}); err == nil {
			t.Fatal("no error")
		}
		if out, _ := s.ListDocsOwners(ctx, &orchdv1.ListDocsOwnersRequest{}); len(out.GetOwners()) != 1 {
			t.Errorf("owners = %+v", out.GetOwners())
		}
	})
	t.Run("purge deletes everything it indexed", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		got, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme", Purge: true, Confirm: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetPurgedSources() != 2 || len(store.deleted) != 2 {
			t.Errorf("got %+v, deleted %v", got, store.deleted)
		}
	})
	t.Run("another spelling removes the owner and its docs", func(t *testing.T) {
		s, store, _ := docsOwnerServer(t)
		seedDocs(t, s)
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "ACME", Purge: true, Confirm: true}); err != nil {
			t.Fatal(err)
		}
		if len(store.deleted) != 2 {
			t.Errorf("deleted %v", store.deleted)
		}
	})
	t.Run("an unknown owner and a bad name", func(t *testing.T) {
		s, _, _ := docsOwnerServer(t)
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme"}); grpcstatus.Code(err) != codes.NotFound {
			t.Errorf("err = %v", err)
		}
		if _, err := s.RemoveDocsOwner(ctx, &orchdv1.RemoveDocsOwnerRequest{Owner: "a/b"}); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("err = %v", err)
		}
	})
}

func TestListDocsOwnerRepos(t *testing.T) {
	ctx := context.Background()
	s, _, _ := docsOwnerServer(t)
	if _, err := s.ListDocsOwnerRepos(ctx, &orchdv1.ListDocsOwnerReposRequest{Owner: "acme"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no GitHub err = %v", err)
	}
	s.docsGH = fakeDocsGH{repos: []docs.Repo{{Name: "a", IsArchived: true}, {Name: "b", IsFork: true}}}
	if _, err := s.ListDocsOwnerRepos(ctx, &orchdv1.ListDocsOwnerReposRequest{Owner: "../x"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad login err = %v", err)
	}
	got, err := s.ListDocsOwnerRepos(ctx, &orchdv1.ListDocsOwnerReposRequest{Owner: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetRepos()) != 2 || !got.GetRepos()[0].GetArchived() || !got.GetRepos()[1].GetFork() {
		t.Errorf("repos = %+v", got.GetRepos())
	}
	s.docsGH = fakeDocsGH{err: errors.New("offline")}
	if _, err := s.ListDocsOwnerRepos(ctx, &orchdv1.ListDocsOwnerReposRequest{Owner: "acme"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("gh failure err = %v", err)
	}
}

func TestRefreshSecs(t *testing.T) {
	for in, want := range map[string]int64{"1h": 3600, "": 21600, "soon": 21600, "-1h": 21600} {
		if got := refreshSecs(in); got != want {
			t.Errorf("refreshSecs(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestListDocsKeepsTheSelectionFromTasks(t *testing.T) {
	ctx := context.Background()
	s, _, _ := docsOwnerServer(t)
	s.docsSync = nil
	if _, err := s.SaveDocsOwner(ctx, &orchdv1.SaveDocsOwnerRequest{Owner: "acme", Enabled: true, RepoMode: "include", Repos: []string{"secret-project"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	o := got.GetOwners()[0]
	if o.GetEnabled() || o.GetRepoMode() != "" || len(o.GetSelectedRepos()) != 0 {
		t.Errorf("ListDocs, which tasks may call, carries the selection: %+v", o)
	}
}
