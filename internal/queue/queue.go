// Package queue wires the go-workflows durable backend, the task-result store,
// and the dispatcher used to create workflow instances. The workflow engine's
// state lives in <data>/workflows.db (pure-Go SQLite, no cgo); task results are
// mirrored into orch.db's tasks table for inspection.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/redact"
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

// SaveResult upserts the terminal status, result and outcome metrics of a task
// (idempotent by id).
func (s *SQLTaskStore) SaveResult(ctx context.Context, in workflows.AgentTaskInput, out workflows.Outcome) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, agent, body, result, status, updated_at,
		                    cost_usd, duration_ms, turns, session_id, models, prs, verification)
		 VALUES (?, ?, ?, ?, ?, datetime('now'), ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   result=excluded.result, status=excluded.status, updated_at=datetime('now'),
		   cost_usd=excluded.cost_usd, duration_ms=excluded.duration_ms, turns=excluded.turns,
		   session_id=excluded.session_id, models=excluded.models, prs=excluded.prs,
		   verification=excluded.verification`,
		in.ID, in.Agent, in.Body, out.Result, out.Status,
		out.CostUSD, out.DurationMS, out.Turns, out.SessionID,
		strings.Join(out.Models, ","), strings.Join(out.PRs, ","), out.Verification)
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
	// Outcome metrics; zero until the task finishes.
	CostUSD      float64
	DurationMS   int64
	Turns        int
	SessionID    string
	Models       []string
	PRs          []string
	Verification string
}

// taskColumns is the select list scanTask reads, in order.
const taskColumns = `id, agent, body, COALESCE(result,''), status, updated_at,
	COALESCE(cost_usd,0), COALESCE(duration_ms,0), COALESCE(turns,0),
	COALESCE(session_id,''), COALESCE(models,''), COALESCE(prs,''), COALESCE(verification,'')`

type rowScanner interface{ Scan(dest ...any) error }

func scanTask(r rowScanner) (Task, error) {
	var t Task
	var models, prs string
	err := r.Scan(&t.ID, &t.Agent, &t.Body, &t.Result, &t.Status, &t.UpdatedAt,
		&t.CostUSD, &t.DurationMS, &t.Turns, &t.SessionID, &models, &prs, &t.Verification)
	t.Models, t.PRs = splitList(models), splitList(prs)
	return t, err
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
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
	q := `SELECT ` + taskColumns + ` FROM tasks`
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
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	// A mid-iteration driver error would otherwise look like a short result set.
	return out, rows.Err()
}

// ActiveCount counts tasks that are pending or in progress.
func (s *SQLTaskStore) ActiveCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE status IN ('pending', 'in_progress')`).Scan(&n)
	return n, err
}

// Get returns one task. found is false when no such ID exists.
func (s *SQLTaskStore) Get(ctx context.Context, id string) (Task, bool, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// taskInput builds a task's durable workflow input.
func taskInput(id, agent, body, cwd string, heads map[string]string) workflows.AgentTaskInput {
	return workflows.AgentTaskInput{ID: id, Agent: agent, Body: body, Cwd: cwd, Heads: heads}
}

// ErrQueueFull rejects a dispatch while too many tasks are pending or running.
var ErrQueueFull = errors.New("queue full")

// Dispatcher creates durable AgentTask workflow instances.
type Dispatcher struct {
	client *client.Client
	store  *SQLTaskStore
	// MaxActive caps tasks pending or in progress; zero means no cap. Queued
	// sessions run unattended, so a task that keeps dispatching more (directly or
	// through injected instructions) must hit a hard ceiling rather than grow
	// without bound.
	MaxActive int
	// Snapshot records the checkouts of the task's repository at dispatch, the
	// baseline verification compares against. Nil records none.
	Snapshot func(ctx context.Context, dir string) map[string]string
}

// NewDispatcher builds a dispatcher over a go-workflows backend. store may be
// nil, in which case tasks are not recorded in the ledger until they finish.
func NewDispatcher(b backend.Backend, store *SQLTaskStore) *Dispatcher {
	return &Dispatcher{client: client.New(b), store: store}
}

// Dispatch enqueues an agent task and returns its durable instance ID. cwd is
// where the agent session will run; empty runs it in a scratch directory.
func (d *Dispatcher) Dispatch(ctx context.Context, agent, body, cwd string) (string, error) {
	if d.store != nil && d.MaxActive > 0 {
		active, err := d.store.ActiveCount(ctx)
		if err != nil {
			return "", err
		}
		if active >= d.MaxActive {
			return "", fmt.Errorf("%w: %d tasks pending or running (queue.max_active=%d)", ErrQueueFull, active, d.MaxActive)
		}
	}
	// The body is stored in the ledger and replayed through queue_read, so it is
	// held to the same redaction as everything else orch persists.
	body, _ = redact.Redact(body)
	id := "task-" + uuid.NewString()
	var heads map[string]string
	if d.Snapshot != nil && cwd != "" {
		heads = d.Snapshot(ctx, cwd)
	}
	in := taskInput(id, agent, body, cwd, heads)
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
