// Package queue wires the go-workflows durable backend, the task-result store,
// and the dispatcher used to create workflow instances. The workflow engine's
// state lives in <data>/workflows.db (pure-Go SQLite, no cgo); task results are
// mirrored into orch.db's tasks table for inspection.
package queue

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/workflows"
)

// NewBackend opens the go-workflows SQLite backend at <data>/workflows.db.
func NewBackend() (backend.Backend, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	path := filepath.Join(paths.DataDir(), "workflows.db")
	return sqlite.NewSqliteBackend(path, sqlite.WithApplyMigrations(true)), nil
}

// SQLTaskStore persists task results into orch.db (satisfies workflows.TaskStore).
type SQLTaskStore struct {
	db *sql.DB
}

// NewSQLTaskStore constructs a task store over an open orch.db handle.
func NewSQLTaskStore(db *sql.DB) *SQLTaskStore { return &SQLTaskStore{db: db} }

// SaveResult upserts the terminal result of a task (idempotent by id).
func (s *SQLTaskStore) SaveResult(ctx context.Context, in workflows.AgentTaskInput, result string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, agent, body, result, status, updated_at)
		 VALUES (?, ?, ?, ?, 'complete', datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		   result=excluded.result, status='complete', updated_at=datetime('now')`,
		in.ID, in.Agent, in.Body, result)
	return err
}

// Dispatcher creates durable AgentTask workflow instances.
type Dispatcher struct {
	client *client.Client
}

// NewDispatcher builds a dispatcher over a go-workflows backend.
func NewDispatcher(b backend.Backend) *Dispatcher {
	return &Dispatcher{client: client.New(b)}
}

// Dispatch enqueues an agent task and returns its durable instance ID.
func (d *Dispatcher) Dispatch(ctx context.Context, agent, body string) (string, error) {
	id := "task-" + uuid.NewString()
	_, err := d.client.CreateWorkflowInstance(ctx,
		client.WorkflowInstanceOptions{InstanceID: id},
		workflows.AgentTask,
		workflows.AgentTaskInput{ID: id, Agent: agent, Body: body},
	)
	if err != nil {
		return "", err
	}
	return id, nil
}
