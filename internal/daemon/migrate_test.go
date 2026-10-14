package daemon

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeMigrator struct {
	rep     rag.MigrateReport
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeMigrator) MigrateHybrid(_ context.Context, target string, apply bool, _ rag.Commit) (rag.MigrateReport, error) {
	if f.started != nil {
		close(f.started)
		<-f.release
	}
	r := f.rep
	r.To, r.Applied = target, apply && f.err == nil
	return r, f.err
}

func TestMigrateHybridRPC(t *testing.T) {
	ctx := context.Background()
	if _, err := (&server{}).MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("no migrator err = %v", err)
	}
	ok := &server{migrator: &fakeMigrator{rep: rag.MigrateReport{From: "a", Sources: 3, OldChunks: 4, NewChunks: 9, Skipped: 1}}}
	resp, err := ok.MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{Target: "b", Apply: true})
	if err != nil || resp.GetFrom() != "a" || resp.GetTo() != "b" || resp.GetNewChunks() != 9 || resp.GetSkipped() != 1 || !resp.GetApplied() {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	// A refused run still returns its report, with the reason.
	failing := &server{migrator: &fakeMigrator{rep: rag.MigrateReport{Oversize: 2, Problems: []string{"repo://big: too large"}},
		err: errors.New("2 source(s) are too large")}}
	resp, err = failing.MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{Apply: true})
	if err != nil || resp.GetError() == "" || resp.GetOversize() != 2 || len(resp.GetProblems()) != 1 || resp.GetApplied() {
		t.Fatalf("refused resp = %+v, %v", resp, err)
	}
	// A second migration while one runs is refused.
	slow := &fakeMigrator{started: make(chan struct{}), release: make(chan struct{})}
	busy := &server{migrator: slow}
	done := make(chan error, 1)
	go func() { _, err := busy.MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{}); done <- err }()
	<-slow.started
	if _, err := busy.MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{}); grpcstatus.Code(err) != codes.Aborted {
		t.Fatalf("concurrent err = %v", err)
	}
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRecordSwitch(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	if err := recordSwitch("orchestrator", "orchestrator-hybrid"); err != nil {
		t.Fatal(err)
	}
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := config.Unmarshal(v)
	if s.RAG.Qdrant.Collection != "orchestrator-hybrid" || s.RAG.Qdrant.BackupCollection != "orchestrator" {
		t.Fatalf("recorded %+v", s.RAG.Qdrant)
	}
}
