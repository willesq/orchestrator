package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cschleiden/go-workflows/backend/sqlite"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/workflows"
)

func TestDispatchRecordsTheRoute(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := NewSQLTaskStore(conn)
	b := sqlite.NewInMemoryBackend(sqlite.WithApplyMigrations(true))
	t.Cleanup(func() { _ = b.Close() })
	d := NewDispatcher(b, store)
	ctx := context.Background()

	rt := workflows.Route{Kind: "code", Provider: "codex", Model: "gpt-sol", Effort: "high"}
	id, err := d.Dispatch(ctx, "dev", "b", "", rt)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "code" || got.Provider != "codex" || got.Model != "gpt-sol" || got.Effort != "high" {
		t.Fatalf("task = %+v", got)
	}
	var created int64
	if err := conn.QueryRow(`SELECT COALESCE(created_at,0) FROM tasks WHERE id = ?`, id).Scan(&created); err != nil || created == 0 {
		t.Fatalf("created_at = %d, %v", created, err)
	}
	unrouted, err := d.Dispatch(ctx, "dev", "b", "", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ = store.Get(ctx, unrouted)
	if got.Kind != "" || got.Model != "" {
		t.Fatalf("unrouted task = %+v", got)
	}
	if in := taskInput("t", "dev", "b", "", nil, rt); in.Route != rt {
		t.Fatalf("taskInput dropped the route: %+v", in)
	}
}

func TestTaskReadsVerdicts(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, rating, pr_outcome)
		VALUES ('t', 'dev', 'b', '', 'complete', datetime('now'), 'good', 'rework')`); err != nil {
		t.Fatal(err)
	}
	got, _, err := NewSQLTaskStore(conn).Get(context.Background(), "t")
	if err != nil || got.Rating != "good" || got.PROutcome != "rework" {
		t.Fatalf("task = %+v, %v", got, err)
	}
}

// TestDispatchRecordsReadSpace: the space orchd puts on the dispatch's
// context is kept in the task's durable input (D-46); none records "".
func TestDispatchRecordsReadSpace(t *testing.T) {
	if got := ReadSpaceOf(context.Background()); got != "" {
		t.Fatalf("a bare context records %q", got)
	}
	ctx := WithReadSpace(context.Background(), "acme")
	if got := ReadSpaceOf(ctx); got != "acme" {
		t.Fatalf("ReadSpaceOf = %q, want acme", got)
	}
	in := taskInput("t", "dev", "b", "", nil, workflows.Route{})
	in.Space = ReadSpaceOf(ctx)
	if in.Space != "acme" {
		t.Fatalf("input space = %q", in.Space)
	}
}
