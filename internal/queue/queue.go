// Package queue wires the go-workflows durable backend, the task-result store,
// and the dispatcher used to create workflow instances. The workflow engine's
// state lives in <data>/workflows.db (pure-Go SQLite, no cgo); task results are
// mirrored into orch.db's tasks table for inspection.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

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

// Create records a task as pending at dispatch time.
//
// Without this a task had no row until it finished, so anything inspecting the
// ledger saw nothing at all while work was outstanding — the queue looked empty
// whether or not something was in flight.
func (s *SQLTaskStore) Create(ctx context.Context, in workflows.AgentTaskInput) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, agent, body, result, status, updated_at)
		 VALUES (?, ?, ?, '', 'pending', datetime('now'))
		 ON CONFLICT(id) DO NOTHING`,
		in.ID, in.Agent, in.Body)
	return err
}

// SetStatus moves a task to a new non-terminal status, leaving its result alone.
func (s *SQLTaskStore) SetStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status=?, updated_at=datetime('now') WHERE id=?`,
		status, id)
	return err
}

// Task is one row of the dispatch ledger.
type Task struct {
	ID        string
	Agent     string
	Body      string
	Result    string
	Status    string
	UpdatedAt string
}

// maxListLimit caps how many task rows one call may return.
const maxListLimit = 500

// ListFilter narrows a task listing.
type ListFilter struct {
	Status string
	Agent  string
	Limit  int
}

// List returns tasks newest first, optionally filtered by status and agent.
func (s *SQLTaskStore) List(ctx context.Context, f ListFilter) ([]Task, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	// Task bodies and results are unbounded TEXT, so an uncapped limit lets one
	// call materialise the whole table into memory and then a protobuf message.
	if f.Limit > maxListLimit {
		f.Limit = maxListLimit
	}
	q := `SELECT id, agent, body, COALESCE(result,''), status, updated_at FROM tasks`
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, f.Agent)
	}
	if len(where) > 0 {
		// #nosec G202 -- every element of `where` is a compile-time literal
		// ("status = ?" / "agent = ?"); caller values are bound via args below.
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Agent, &t.Body, &t.Result, &t.Status, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	// A mid-iteration driver error would otherwise look like a short result set.
	return out, rows.Err()
}

// Get returns one task. found is false when no such ID exists.
func (s *SQLTaskStore) Get(ctx context.Context, id string) (Task, bool, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent, body, COALESCE(result,''), status, updated_at FROM tasks WHERE id=?`,
		id).Scan(&t.ID, &t.Agent, &t.Body, &t.Result, &t.Status, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// Dispatcher creates durable AgentTask workflow instances.
type Dispatcher struct {
	client *client.Client
	store  *SQLTaskStore
}

// NewDispatcher builds a dispatcher over a go-workflows backend. store may be
// nil, in which case tasks are not recorded in the ledger until they finish.
func NewDispatcher(b backend.Backend, store *SQLTaskStore) *Dispatcher {
	return &Dispatcher{client: client.New(b), store: store}
}

// Dispatch enqueues an agent task and returns its durable instance ID.
func (d *Dispatcher) Dispatch(ctx context.Context, agent, body string) (string, error) {
	id := "task-" + uuid.NewString()
	in := workflows.AgentTaskInput{ID: id, Agent: agent, Body: body}
	// Record before enqueuing: a row with no workflow is a visible, diagnosable
	// state, whereas a workflow with no row is invisible to every reader.
	if d.store != nil {
		if err := d.store.Create(ctx, in); err != nil {
			return "", err
		}
	}
	_, err := d.client.CreateWorkflowInstance(ctx,
		client.WorkflowInstanceOptions{InstanceID: id},
		workflows.AgentTask, in,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}
