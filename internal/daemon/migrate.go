package daemon

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// hybridMigrator moves the RAG store to the hybrid layout.
type hybridMigrator interface {
	MigrateHybrid(ctx context.Context, target string, apply bool, commit rag.Commit) (rag.MigrateReport, error)
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
