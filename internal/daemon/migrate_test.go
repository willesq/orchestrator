package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	progress *rag.MigrationProgress
	dropErr  error
	dropped  []string // collections dropped, with apply
	planned  []string // collections planned, without apply

	reindex    rag.ReindexReport
	reindexErr error
	reindexed  []bool // apply flag of each re-index call
}

func (f *fakeMigrator) ReindexKeywords(_ context.Context, apply bool) (rag.ReindexReport, error) {
	f.reindexed = append(f.reindexed, apply)
	rep := f.reindex
	rep.Applied = apply
	return rep, f.reindexErr
}

func (f *fakeMigrator) MigrationProgress() (rag.MigrationProgress, bool) {
	if f.progress == nil {
		return rag.MigrationProgress{}, false
	}
	return *f.progress, true
}

func (f *fakeMigrator) DropCollection(_ context.Context, name string, apply bool) (rag.DropReport, error) {
	if f.dropErr != nil {
		return rag.DropReport{}, f.dropErr
	}
	if apply {
		f.dropped = append(f.dropped, name)
	} else {
		f.planned = append(f.planned, name)
	}
	return rag.DropReport{Collection: name, Points: 7, Dropped: apply}, nil
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
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
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

func TestDropBackupRPC(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if _, err := (&server{}).DropBackup(ctx, &orchdv1.DropBackupRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("no migrator err = %v", err)
	}
	m := &fakeMigrator{}
	s := &server{migrator: m}
	if _, err := s.DropBackup(ctx, &orchdv1.DropBackupRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no backup err = %v", err)
	}
	if err := recordSwitch("orchestrator", "orchestrator-hybrid"); err != nil {
		t.Fatal(err)
	}
	resp, err := s.DropBackup(ctx, &orchdv1.DropBackupRequest{})
	if err != nil || resp.GetCollection() != "orchestrator" || resp.GetPoints() != 7 || resp.GetDropped() || len(m.dropped) != 0 {
		t.Fatalf("plan = %+v, %v, dropped %v", resp, err, m.dropped)
	}
	resp, err = s.DropBackup(ctx, &orchdv1.DropBackupRequest{Apply: true})
	if err != nil || !resp.GetDropped() || len(m.dropped) != 1 || m.dropped[0] != "orchestrator" {
		t.Fatalf("apply = %+v, %v, dropped %v", resp, err, m.dropped)
	}
	v, _ := config.Load()
	st, _ := config.Unmarshal(v)
	if st.RAG.Qdrant.BackupCollection != "" || st.RAG.Qdrant.Collection != "orchestrator-hybrid" {
		t.Fatalf("config after drop = %+v", st.RAG.Qdrant)
	}
	// With the backup forgotten, there is nothing left to drop.
	if _, err := s.DropBackup(ctx, &orchdv1.DropBackupRequest{Apply: true}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second drop err = %v", err)
	}
}

func TestDropBackupRefusals(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := recordSwitch("orchestrator", "orchestrator-hybrid"); err != nil {
		t.Fatal(err)
	}
	refusing := &server{migrator: &fakeMigrator{dropErr: errors.New("orchd is using it")}}
	if _, err := refusing.DropBackup(ctx, &orchdv1.DropBackupRequest{Apply: true}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("refused err = %v", err)
	}
	v, _ := config.Load()
	if st, _ := config.Unmarshal(v); st.RAG.Qdrant.BackupCollection != "orchestrator" {
		t.Fatal("a refused drop forgot the backup")
	}
	// Config naming the same collection twice is refused before any drop.
	if err := config.Save(map[string]any{"rag.qdrant.backup_collection": "orchestrator-hybrid"}, "test"); err != nil {
		t.Fatal(err)
	}
	m := &fakeMigrator{}
	if _, err := (&server{migrator: m}).DropBackup(ctx, &orchdv1.DropBackupRequest{Apply: true}); grpcstatus.Code(err) != codes.FailedPrecondition || len(m.dropped) != 0 {
		t.Fatalf("same-name err = %v, dropped %v", err, m.dropped)
	}
	// A drop waits for no migration: it is refused while one runs.
	migrating.Lock()
	_, err := (&server{migrator: m}).DropBackup(ctx, &orchdv1.DropBackupRequest{})
	migrating.Unlock()
	if grpcstatus.Code(err) != codes.Aborted {
		t.Fatalf("during migration err = %v", err)
	}
}

func TestStatusReportsMigrationProgress(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	s := &server{start: time.Now(), migrator: &fakeMigrator{progress: &rag.MigrationProgress{
		Target: "orchestrator-hybrid", Done: 120, Total: 664, Chunks: 5012, Started: started}}}
	resp, err := s.Status(context.Background(), &orchdv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.GetMigration()
	if m.GetTarget() != "orchestrator-hybrid" || m.GetSourcesDone() != 120 || m.GetSourcesTotal() != 664 ||
		m.GetChunks() != 5012 || m.GetStartedAt() != started.Unix() {
		t.Fatalf("migration = %+v", m)
	}
	idle := &server{start: time.Now(), migrator: &fakeMigrator{}}
	if resp, _ := idle.Status(context.Background(), &orchdv1.StatusRequest{}); resp.GetMigration() != nil {
		t.Fatalf("idle migration = %+v", resp.GetMigration())
	}
}

func TestDropBackupAlreadyGone(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := recordSwitch("orchestrator", "orchestrator-hybrid"); err != nil {
		t.Fatal(err)
	}
	gone := &server{migrator: &fakeMigrator{dropErr: fmt.Errorf("%w: %q", rag.ErrNoCollection, "orchestrator")}}
	// Planning says so and changes nothing.
	if _, err := gone.DropBackup(ctx, &orchdv1.DropBackupRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("plan err = %v", err)
	}
	if _, backup := backupOf(t); backup != "orchestrator" {
		t.Fatalf("a plan forgot the backup: %q", backup)
	}
	// Applying forgets the backup an earlier drop already deleted.
	resp, err := gone.DropBackup(ctx, &orchdv1.DropBackupRequest{Apply: true})
	if err != nil || !resp.GetDropped() {
		t.Fatalf("apply = %+v, %v", resp, err)
	}
	if _, backup := backupOf(t); backup != "" {
		t.Fatalf("backup still recorded: %q", backup)
	}
}

func TestDropBackupConfigClearFails(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := recordSwitch("orchestrator", "orchestrator-hybrid"); err != nil {
		t.Fatal(err)
	}
	orig := clearBackup
	t.Cleanup(func() { clearBackup = orig })
	clearBackup = func() error { return errors.New("disk full") }
	_, err := (&server{migrator: &fakeMigrator{}}).DropBackup(context.Background(), &orchdv1.DropBackupRequest{Apply: true})
	if err == nil || !strings.Contains(err.Error(), "clearing rag.qdrant.backup_collection failed") {
		t.Fatalf("err = %v", err)
	}
}

func backupOf(t *testing.T) (string, string) {
	t.Helper()
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := config.Unmarshal(v)
	return s.RAG.Qdrant.Collection, s.RAG.Qdrant.BackupCollection
}
