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
