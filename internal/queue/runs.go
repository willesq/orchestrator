package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/converter"
	"github.com/cschleiden/go-workflows/backend/history"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/core"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/workflows"
)

// Runs reads and starts the workflow runs behind tasks. A task's first run is
// the workflow instance named after it; a retry is another instance named
// <task id>#retry-<n>. Its newest run decides whether anything still owns it.
type Runs struct {
	// db is a read-only handle on the backend's own database, used only to
	// find a task's instances by name.
	//
	// Reason: every backend call needs an instance's execution ID as well as
	// its name, and go-workflows has no lookup by name. The query reads the
	// instances table of the go-workflows version pinned in go.mod; the tests
	// run it against the real backend so an upgrade that changes it fails.
	db      *sql.DB
	backend backend.Backend
	client  *client.Client
}

// OpenRuns opens the run index over the backend b stored at path.
func OpenRuns(path string, b backend.Backend) (*Runs, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Runs{db: db, backend: b, client: client.New(b)}, nil
}

// Close releases the read-only handle.
func (r *Runs) Close() error { return r.db.Close() }

// Run is one workflow run of a task.
type Run struct {
	Instance *workflow.Instance
	// Active is true while the workflow has not finished: it is running, or
	// waiting for a worker to pick it up again.
	Active bool
}

const retrySuffix = "#retry-"

// Latest returns the task's newest run, and false when it has none.
func (r *Runs) Latest(ctx context.Context, taskID string) (Run, bool, error) {
	prefix := taskID + retrySuffix
	var id, exec string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, execution_id FROM instances
		 WHERE id = ? OR substr(id, 1, ?) = ?
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		taskID, len(prefix), prefix).Scan(&id, &exec)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("find runs of %s: %w", taskID, err)
	}
	inst := &workflow.Instance{InstanceID: id, ExecutionID: exec}
	state, err := r.backend.GetWorkflowInstanceState(ctx, inst)
	if err != nil {
		return Run{}, false, fmt.Errorf("state of %s: %w", id, err)
	}
	return Run{Instance: inst, Active: state == core.WorkflowInstanceStateActive}, true, nil
}

// Input is the task input its runs were started with, from the newest run
// whose history records it.
//
// Reason: a run no worker has picked up yet keeps its start event in the
// backend's pending queue, not in its history, so a never-started retry must
// not hide the input an earlier run recorded.
func (r *Runs) Input(ctx context.Context, taskID string) (workflows.AgentTaskInput, error) {
	var in workflows.AgentTaskInput
	prefix := taskID + retrySuffix
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, execution_id FROM instances
		 WHERE id = ? OR substr(id, 1, ?) = ?
		 ORDER BY created_at DESC, rowid DESC`,
		taskID, len(prefix), prefix)
	if err != nil {
		return in, fmt.Errorf("find runs of %s: %w", taskID, err)
	}
	var insts []*workflow.Instance
	for rows.Next() {
		var id, exec string
		if err := rows.Scan(&id, &exec); err != nil {
			_ = rows.Close()
			return in, err
		}
		insts = append(insts, &workflow.Instance{InstanceID: id, ExecutionID: exec})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return in, err
	}
	for _, inst := range insts {
		ok, err := r.startInput(ctx, inst, &in)
		if err != nil || ok {
			return in, err
		}
	}
	return in, fmt.Errorf("no run of %s has a recorded input", taskID)
}

// RecordedInput is the task input as Input reads it, or, for a task no
// worker has picked up yet, from its first run's start event still in the
// backend's pending queue. Unlike Input it therefore knows a waiting task's
// input, such as the space it recorded at dispatch (SP-026).
func (r *Runs) RecordedInput(ctx context.Context, taskID string) (workflows.AgentTaskInput, error) {
	in, err := r.Input(ctx, taskID)
	if err == nil {
		return in, nil
	}
	// Reason: the query reads the pending_events and attributes tables of
	// the go-workflows version pinned in go.mod, as the instance lookup
	// reads its instances table; the tests run it against the real
	// backend.
	var data []byte
	qerr := r.db.QueryRowContext(ctx,
		`SELECT a.data FROM pending_events pe
		 JOIN attributes a ON a.id = pe.id AND a.instance_id = pe.instance_id AND a.execution_id = pe.execution_id
		 WHERE pe.instance_id = ? AND pe.event_type = ? LIMIT 1`,
		taskID, int(history.EventType_WorkflowExecutionStarted)).Scan(&data)
	if qerr != nil {
		return in, err
	}
	attrs, derr := history.DeserializeAttributes(history.EventType_WorkflowExecutionStarted, data)
	a, ok := attrs.(*history.ExecutionStartedAttributes)
	if derr != nil || !ok || len(a.Inputs) == 0 {
		return in, err
	}
	var pending workflows.AgentTaskInput
	if cerr := converter.DefaultConverter.From(a.Inputs[0], &pending); cerr != nil {
		return in, fmt.Errorf("decode the pending input of %s: %w", taskID, cerr)
	}
	return pending, nil
}

// startInput decodes the input recorded in inst's start event into in.
func (r *Runs) startInput(ctx context.Context, inst *workflow.Instance, in *workflows.AgentTaskInput) (bool, error) {
	events, err := r.backend.GetWorkflowInstanceHistory(ctx, inst, nil)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Type != history.EventType_WorkflowExecutionStarted {
			continue
		}
		a, ok := e.Attributes.(*history.ExecutionStartedAttributes)
		if !ok || len(a.Inputs) == 0 {
			return false, nil
		}
		if err := converter.DefaultConverter.From(a.Inputs[0], in); err != nil {
			return false, fmt.Errorf("decode the input of %s: %w", inst.InstanceID, err)
		}
		return true, nil
	}
	return false, nil
}

// Started reports whether a worker ever picked the run up (its history
// records its start).
func (r *Runs) Started(ctx context.Context, run Run) (bool, error) {
	var in workflows.AgentTaskInput
	return r.startInput(ctx, run.Instance, &in)
}

// Retries counts the retry runs a task has had.
func (r *Runs) Retries(ctx context.Context, taskID string) (int, error) {
	prefix := taskID + retrySuffix
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE substr(id, 1, ?) = ?`,
		len(prefix), prefix).Scan(&n)
	return n, err
}

// Failure is why a finished run failed, or "" when it completed or has not
// finished.
func (r *Runs) Failure(ctx context.Context, run Run) (string, error) {
	events, err := r.backend.GetWorkflowInstanceHistory(ctx, run.Instance, nil)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case history.EventType_WorkflowExecutionFinished:
			if a, ok := events[i].Attributes.(*history.ExecutionCompletedAttributes); ok && a.Error != nil {
				return a.Error.Error(), nil
			}
			return "", nil
		case history.EventType_WorkflowExecutionCanceled:
			return "canceled", nil
		case history.EventType_WorkflowExecutionTerminated:
			return "terminated", nil
		}
	}
	return "", nil
}

// Start begins another run of an existing task. The runner resumes the task's
// session when one exists, so a retry continues rather than starts over.
func (r *Runs) Start(ctx context.Context, in workflows.AgentTaskInput) (string, error) {
	id := in.ID + retrySuffix + uuid.NewString()[:8]
	if _, err := r.client.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: id},
		workflows.AgentTask, in); err != nil {
		return "", err
	}
	return id, nil
}

// Cancel cancels an active run.
func (r *Runs) Cancel(ctx context.Context, run Run) error {
	return r.client.CancelWorkflowInstance(ctx, run.Instance)
}
