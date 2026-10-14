package cli

import (
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
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
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetMany(v, map[string]any{"rag.qdrant.collection": "orchestrator-hybrid", "rag.qdrant.backup_collection": "orchestrator"}); err != nil {
		t.Fatal(err)
	}
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
	t.Setenv(agentrun.TaskIDEnv, "")
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Set(v, "rag.qdrant.backup_collection", "../x"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "rag", "migrate-hybrid", "--rollback"); err == nil || !strings.Contains(err.Error(), "not a valid name") {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackWithoutBackup(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv(agentrun.TaskIDEnv, "")
	if _, err := run(factory(&fakeClient{}, nil), "", "rag", "migrate-hybrid", "--rollback"); err == nil ||
		!strings.Contains(err.Error(), "no backup") {
		t.Fatalf("err = %v", err)
	}
}
