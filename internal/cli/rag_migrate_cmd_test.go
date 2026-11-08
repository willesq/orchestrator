package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/config/configtest"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func collections(t *testing.T) (string, string) {
	t.Helper()
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return s.RAG.Qdrant.Collection, s.RAG.Qdrant.BackupCollection
}

func TestRAGMigrateHybrid(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	orig := fakeMigrate
	t.Cleanup(func() { fakeMigrate = orig })
	var got *orchdv1.MigrateHybridRequest
	fakeMigrate = func(req *orchdv1.MigrateHybridRequest) (*orchdv1.MigrateHybridResponse, error) {
		got = req
		return &orchdv1.MigrateHybridResponse{From: "orchestrator", To: "orchestrator-hybrid", Sources: 3, OldChunks: 4,
			NewChunks: 9, Skipped: 1, Applied: req.GetApply()}, nil
	}
	cf := factory(&fakeClient{}, nil)

	out, err := run(cf, "", "rag", "migrate-hybrid")
	if err != nil || got.GetApply() || !strings.Contains(out, "plan only") || !strings.Contains(out, "9 with token-sized chunks") {
		t.Fatalf("plan: %q, %v (req %+v)", out, err, got)
	}
	if c, _ := collections(t); c != "orchestrator" {
		t.Fatalf("a plan changed config: %s", c)
	}
	out, err = run(cf, "", "rag", "migrate-hybrid", "--apply", "--target", "orchestrator-hybrid")
	if err != nil || !got.GetApply() || got.GetTarget() != "orchestrator-hybrid" || !strings.Contains(out, "migrated") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	// orchd records the switch itself; the CLI leaves config alone.
	if c, _ := collections(t); c != "orchestrator" {
		t.Fatalf("the CLI wrote config on apply: %s", c)
	}
	configtest.Set(t, map[string]string{"rag.qdrant.collection": `"orchestrator-hybrid"`, "rag.qdrant.backup_collection": `"orchestrator"`})
	out, err = run(cf, "", "rag", "migrate-hybrid", "--rollback")
	if err != nil || !strings.Contains(out, "orch restart") {
		t.Fatalf("rollback: %q, %v", out, err)
	}
	if c, b := collections(t); c != "orchestrator" || b != "orchestrator-hybrid" {
		t.Fatalf("after rollback: collection %s, backup %s", c, b)
	}
	if _, err := run(cf, "", "rag", "migrate-hybrid", "--rollback", "--apply"); err == nil {
		t.Fatal("--rollback with --apply accepted")
	}
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	if _, err := run(cf, "", "rag", "migrate-hybrid", "--apply"); err == nil {
		t.Fatal("a queued task may not migrate")
	}
}

func TestRAGMigrateReportsProblems(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	orig := fakeMigrate
	t.Cleanup(func() { fakeMigrate = orig })
	fakeMigrate = func(*orchdv1.MigrateHybridRequest) (*orchdv1.MigrateHybridResponse, error) {
		return &orchdv1.MigrateHybridResponse{From: "a", To: "b", Malformed: 1, Orphans: 2, Removed: 3, Oversize: 4,
			Problems: []string{"repo://x\x1b[2J: chunk indices missing"}, Error: "4 source(s) are too large"}, nil
	}
	out, err := run(factory(&fakeClient{}, nil), "", "rag", "migrate-hybrid", "--apply")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("a refused run must fail after reporting: %v", err)
	}
	for _, want := range []string{"1 with untrusted chunk order, kept whole in point-id order", "2 chunk(s) with no source URI", "3 source(s) left by an earlier run", "4 source(s) too large", "repo://x?[2J"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestRollbackRejectsBadBackup(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	configtest.Set(t, map[string]string{"rag.qdrant.backup_collection": `"../x"`})
	if _, err := run(factory(&fakeClient{}, nil), "", "rag", "migrate-hybrid", "--rollback"); err == nil || !strings.Contains(err.Error(), "not a valid name") {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackWithoutBackup(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	if _, err := run(factory(&fakeClient{}, nil), "", "rag", "migrate-hybrid", "--rollback"); err == nil ||
		!strings.Contains(err.Error(), "no backup") {
		t.Fatalf("err = %v", err)
	}
}

func TestRAGMigrateDropBackup(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	orig := fakeDropBackup
	t.Cleanup(func() { fakeDropBackup = orig })
	var got []bool
	fakeDropBackup = func(req *orchdv1.DropBackupRequest) (*orchdv1.DropBackupResponse, error) {
		got = append(got, req.GetApply())
		return &orchdv1.DropBackupResponse{Collection: "orchestrator", Points: 6675, Dropped: req.GetApply()}, nil
	}
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "rag", "migrate-hybrid", "--drop-backup")
	if err != nil || !strings.Contains(out, "would drop the backup collection orchestrator (6675 points)") || !strings.Contains(out, "plan only") {
		t.Fatalf("plan: %q, %v", out, err)
	}
	out, err = run(cf, "", "rag", "migrate-hybrid", "--drop-backup", "--apply")
	if err != nil || !strings.Contains(out, "dropped the backup collection orchestrator") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("requests = %v", got)
	}
	if _, err := run(cf, "", "rag", "migrate-hybrid", "--drop-backup", "--target", "x"); err == nil {
		t.Fatal("--drop-backup took --target")
	}
	fakeDropBackup = func(*orchdv1.DropBackupRequest) (*orchdv1.DropBackupResponse, error) {
		return nil, errors.New("orchd is using it")
	}
	if _, err := run(cf, "", "rag", "migrate-hybrid", "--drop-backup", "--apply"); err == nil || !strings.Contains(err.Error(), "using") {
		t.Fatalf("refused err = %v", err)
	}
	if _, err := run(factory(nil, errors.New("no daemon")), "", "rag", "migrate-hybrid", "--drop-backup"); err == nil {
		t.Fatal("no daemon gave no error")
	}
}
