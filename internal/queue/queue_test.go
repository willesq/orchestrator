package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cschleiden/go-workflows/backend/sqlite"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/workflows"
)

func TestSQLTaskStoreSaveResult(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		result string
	}{
		{name: "insert", id: "task-1", result: "processed: a"},
		{name: "upsert same id", id: "task-1", result: "processed: b"},
	}
	// Shared db across subtests to exercise the ON CONFLICT upsert path.
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := NewSQLTaskStore(conn)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := workflows.AgentTaskInput{ID: tt.id, Agent: "dev", Body: "a"}
			if err := store.SaveResult(context.Background(), in, tt.result); err != nil {
				t.Fatalf("SaveResult: %v", err)
			}
			var result, status string
			row := conn.QueryRow("SELECT result, status FROM tasks WHERE id=?", tt.id)
			if err := row.Scan(&result, &status); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if result != tt.result {
				t.Errorf("result = %q, want %q", result, tt.result)
			}
			if status != "complete" {
				t.Errorf("status = %q, want complete", status)
			}
		})
	}
}

func TestNewBackend(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "creates workflows backend"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			b, err := NewBackend()
			if err != nil {
				t.Fatalf("NewBackend: %v", err)
			}
			t.Cleanup(func() { _ = b.Close() })
			if b == nil {
				t.Fatalf("nil backend")
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		body  string
	}{
		{name: "creates a workflow instance", agent: "dev", body: "fix it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := sqlite.NewInMemoryBackend(sqlite.WithApplyMigrations(true))
			t.Cleanup(func() { _ = b.Close() })
			d := NewDispatcher(b)

			id, err := d.Dispatch(context.Background(), tt.agent, tt.body)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if id == "" {
				t.Errorf("empty instance id")
			}
		})
	}
}
