package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeDocsSync struct {
	p        docs.Progress
	requests int
}

func (f *fakeDocsSync) Progress() docs.Progress { return f.p }
func (f *fakeDocsSync) Running() bool           { return f.p.Running }

// Request refuses while Running, like the scheduler, and otherwise prepares.
func (f *fakeDocsSync) Request(prepare func() error) error {
	if f.p.Running {
		return docs.ErrBusy
	}
	if err := prepare(); err != nil {
		return err
	}
	f.requests++
	return nil
}

func TestSyncDocs(t *testing.T) {
	ctx := context.Background()
	s, _, sup := catalogServer(t)
	syncer := &fakeDocsSync{}
	s.docsSync = syncer

	// A running sync refuses the request: nothing is queued, prepared or
	// restarted, so the running sync is not interrupted.
	syncer.p = docs.Progress{Running: true, Owner: "willesq", StartedAt: time.Now().Add(-time.Minute), Done: 10, Total: 52}
	_, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "willesq", Force: true})
	if code(err) != codes.Aborted || !strings.Contains(err.Error(), "not queued") || !strings.Contains(err.Error(), "10 of 52") {
		t.Fatalf("busy err = %v", err)
	}
	if sha, _ := s.docsReg.RepoSHA(ctx, "willesq", "orchestrator"); sha == "" {
		t.Error("a refused force request cleared the caches")
	}
	syncer.p = docs.Progress{Running: true, StartedAt: time.Now()}
	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{}); code(err) != codes.Aborted || !strings.Contains(err.Error(), "looking up due owners") {
		t.Errorf("busy before an owner err = %v", err)
	}
	syncer.p = docs.Progress{Running: true, Owner: "willesq", StartedAt: time.Now()}
	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{}); code(err) != codes.Aborted || !strings.Contains(err.Error(), "willesq, listing repos") {
		t.Errorf("busy while listing err = %v", err)
	}

	syncer.p = docs.Progress{Owner: "willesq", FinishedAt: time.Unix(500, 0)}
	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "nobody"}); code(err) != codes.NotFound {
		t.Errorf("unknown owner err = %v", err)
	}
	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "bad owner"}); code(err) != codes.InvalidArgument {
		t.Errorf("invalid owner err = %v", err)
	}

	// Force clears the caches, marks due and starts the unit.
	st, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "willesq", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetOwner() != "willesq" || st.GetFinishedAt() != 500 {
		t.Errorf("state = %v", st)
	}
	if syncer.requests != 1 || len(sup.restarted) != 0 {
		t.Errorf("requests = %d, restarts = %v; want one request and no restart", syncer.requests, sup.restarted)
	}
	if sha, _ := s.docsReg.RepoSHA(ctx, "willesq", "orchestrator"); sha != "" {
		t.Errorf("force kept the repo cache: %q", sha)
	}
	owners, _ := s.docsReg.ListOwners(ctx)
	if owners[0].LastSynced != 0 {
		t.Errorf("owner not marked due: %+v", owners[0])
	}

	// Every owner, without force, keeps the caches.
	_ = s.docsReg.SetRepoState(ctx, docs.RepoState{Owner: "willesq", Repo: "orchestrator", HeadSHA: sha})

	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.docsReg.RepoSHA(ctx, "willesq", "orchestrator"); got != sha {
		t.Errorf("plain sync cleared the cache: %q", got)
	}

	// A preparation that fails fails the request.
	_ = s.docsReg.RemoveOwner(ctx, "willesq")
	if _, err := s.SyncDocs(ctx, &orchdv1.SyncDocsRequest{Owner: "willesq"}); code(err) != codes.NotFound {
		t.Errorf("removed owner err = %v", err)
	}
	if _, err := (&server{}).SyncDocs(ctx, &orchdv1.SyncDocsRequest{}); code(err) != codes.Unavailable {
		t.Errorf("no syncer err = %v", err)
	}
}

func TestGetDocsSyncAndListDocsCarryProgressAndErrors(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	started := time.Unix(1000, 0)
	s.docsSync = &fakeDocsSync{p: docs.Progress{Running: true, Owner: "willesq", StartedAt: started, Done: 3, Total: 52, Repo: "airflow"}}

	st, err := s.GetDocsSync(ctx, &orchdv1.GetDocsSyncRequest{})
	if err != nil || !st.GetRunning() || st.GetStartedAt() != 1000 || st.GetDone() != 3 || st.GetTotal() != 52 || st.GetRepo() != "airflow" {
		t.Fatalf("state = %v, %v", st, err)
	}
	if _, err := (&server{}).GetDocsSync(ctx, nil); code(err) != codes.Unavailable {
		t.Errorf("no syncer err = %v", err)
	}

	_ = s.docsReg.SetRepoState(ctx, docs.RepoState{Owner: "willesq", Repo: "airflow", Status: "error", Error: "clone failed (network): …"})
	_ = s.docsReg.MarkSynced(ctx, "willesq", 2000, "partial", "1 of 2 repos failed")
	r.indexed = nil
	resp, err := s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetSync().GetRunning() || resp.GetOwners()[0].GetError() != "1 of 2 repos failed" {
		t.Errorf("owners/sync = %v / %v", resp.GetOwners(), resp.GetSync())
	}
	var airflow *orchdv1.DocRepo
	for _, rp := range resp.GetOwners()[0].GetRepos() {
		if rp.GetRepo() == "airflow" {
			airflow = rp
		}
	}
	if airflow == nil || !strings.Contains(airflow.GetError(), "network") {
		t.Errorf("airflow = %v", airflow)
	}
}
