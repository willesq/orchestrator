package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// hybridMigrator moves the RAG store to the hybrid layout and cleans up after.
type hybridMigrator interface {
	MigrateHybrid(ctx context.Context, target string, apply bool, commit rag.Commit) (rag.MigrateReport, error)
	MigrationProgress() (rag.MigrationProgress, bool)
	DropCollection(ctx context.Context, name string, apply bool) (rag.DropReport, error)
	ReindexKeywords(ctx context.Context, apply bool) (rag.ReindexReport, error)
}

// migrating allows one migration at a time: two would both dual-write and race
// to switch the active collection.
var migrating sync.Mutex

func (s *server) MigrateHybrid(ctx context.Context, req *orchdv1.MigrateHybridRequest) (*orchdv1.MigrateHybridResponse, error) {
	if s.migrator == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "migration is unavailable")
	}
	if !migrating.TryLock() {
		return nil, grpcstatus.Error(codes.Aborted, "a migration is already running")
	}
	defer migrating.Unlock()
	rep, err := s.migrator.MigrateHybrid(ctx, req.GetTarget(), req.GetApply(), recordSwitch)
	resp := &orchdv1.MigrateHybridResponse{
		From: rep.From, To: rep.To, Sources: clampInt32(rep.Sources), OldChunks: clampInt32(rep.OldChunks),
		NewChunks: clampInt32(rep.NewChunks), Skipped: clampInt32(rep.Skipped), Applied: rep.Applied,
		Malformed: clampInt32(rep.Malformed), Orphans: clampInt32(rep.Orphans), Removed: clampInt32(rep.Removed),
		Oversize: clampInt32(rep.Oversize), Problems: rep.Problems,
	}
	// A refused or failed run still reports what it found, so the problems
	// that stopped it can be fixed.
	if err != nil {
		resp.Error = err.Error()
	}
	return resp, nil
}

// ReindexKeywords brings the keyword vectors up to the current tokenizer. It
// shares the migration lock: a re-index never races a migration's copy.
func (s *server) ReindexKeywords(ctx context.Context, req *orchdv1.ReindexKeywordsRequest) (*orchdv1.ReindexKeywordsResponse, error) {
	if s.migrator == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "re-indexing is unavailable")
	}
	if !migrating.TryLock() {
		return nil, grpcstatus.Error(codes.Aborted, "a hybrid migration, backup drop or keyword re-index is already running")
	}
	defer migrating.Unlock()
	rep, err := s.migrator.ReindexKeywords(ctx, req.GetApply())
	resp := &orchdv1.ReindexKeywordsResponse{
		Collection: rep.Collection, TokenizerVersion: int32(rep.Version), Total: int64(rep.Total),
		Stale: int64(rep.Stale), Updated: int64(rep.Updated), Applied: rep.Applied,
	}
	if err != nil {
		if !req.GetApply() || rep.Collection == "" {
			return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
		}
		resp.Error = err.Error()
	}
	return resp, nil
}

// recordSwitch persists a finished migration in config before orchd switches,
// so a restart keeps using the new collection and --rollback knows the old one.
var recordSwitch rag.Commit = func(from, to string) error {
	v, err := config.Load()
	if err != nil {
		return err
	}
	return config.SetMany(v, map[string]any{
		"rag.qdrant.collection":        to,
		"rag.qdrant.backup_collection": from,
	})
}

// DropBackup deletes the backup collection recorded in config, then forgets
// it. The name never comes from the caller, so this cannot delete arbitrary
// collections. It shares the migration lock: a drop never races a migration.
func (s *server) DropBackup(ctx context.Context, req *orchdv1.DropBackupRequest) (*orchdv1.DropBackupResponse, error) {
	if s.migrator == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "migration is unavailable")
	}
	if !migrating.TryLock() {
		return nil, grpcstatus.Error(codes.Aborted, "a migration is running")
	}
	defer migrating.Unlock()
	v, err := config.Load()
	if err != nil {
		return nil, err
	}
	st, err := config.Unmarshal(v)
	if err != nil {
		return nil, err
	}
	backup := st.RAG.Qdrant.BackupCollection
	if backup == "" {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "no backup collection recorded; nothing to drop")
	}
	if backup == st.RAG.Qdrant.Collection {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "config names %q as both the collection and its backup", backup)
	}
	rep, err := s.migrator.DropCollection(ctx, backup, req.GetApply())
	switch {
	case errors.Is(err, rag.ErrNoCollection) && req.GetApply():
		// An earlier drop deleted it but could not clear config: finish that.
		rep = rag.DropReport{Collection: backup, Dropped: true}
	case errors.Is(err, rag.ErrNoCollection):
		return nil, grpcstatus.Errorf(codes.FailedPrecondition,
			"the backup collection %q no longer exists; --drop-backup --apply forgets it", backup)
	case err != nil:
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	if rep.Dropped {
		// Reload so edits made while the collection was counted and deleted survive.
		if err := clearBackup(); err != nil {
			return nil, fmt.Errorf("dropped %s, but clearing rag.qdrant.backup_collection failed: %w", backup, err)
		}
	}
	return &orchdv1.DropBackupResponse{Collection: rep.Collection, Points: int64(rep.Points), Dropped: rep.Dropped}, nil
}

// clearBackup forgets the recorded backup collection.
var clearBackup = func() error {
	v, err := config.Load()
	if err != nil {
		return err
	}
	return config.SetMany(v, map[string]any{"rag.qdrant.backup_collection": ""})
}

// migrationProgress converts a running migration's progress for Status.
func (s *server) migrationProgress() *orchdv1.MigrationProgress {
	if s.migrator == nil {
		return nil
	}
	p, ok := s.migrator.MigrationProgress()
	if !ok {
		return nil
	}
	return &orchdv1.MigrationProgress{
		Target: p.Target, SourcesDone: clampInt32(p.Done), SourcesTotal: clampInt32(p.Total),
		Chunks: clampInt32(p.Chunks), StartedAt: unixOrZero(p.Started),
	}
}
