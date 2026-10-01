// Package workflows defines the durable go-workflows definitions. Workflow bodies
// must be deterministic; all non-deterministic work (I/O, persistence) happens in
// activities. The persistence dependency is injected via SetStore so activities
// stay testable and the package has no direct database import.
package workflows

import (
	"context"
	"time"

	"github.com/cschleiden/go-workflows/workflow"
)

// AgentTaskInput is the durable input to an agent task.
type AgentTaskInput struct {
	ID    string
	Agent string
	Body  string
}

// WorkDelay is a durable pause before processing. It demonstrates that a task
// in-flight survives an orchd restart: the timer is persisted in workflows.db and
// resumes after the daemon comes back. Overridable (e.g. shortened in tests).
var WorkDelay = 3 * time.Second

// AgentTask is the durable workflow for a dispatched task: mark it running,
// pause, then process.
//
// NOTE: ProcessActivity does not execute the task — see its doc comment. The
// status transitions here are real; the payload is not.
func AgentTask(ctx workflow.Context, in AgentTaskInput) (string, error) {
	workflow.Logger(ctx).Info("agent task started", "id", in.ID, "agent", in.Agent)
	if _, err := workflow.ExecuteActivity[string](
		ctx, workflow.DefaultActivityOptions, StartActivity, in).Get(ctx); err != nil {
		return "", err
	}
	if err := workflow.Sleep(ctx, WorkDelay); err != nil {
		return "", err
	}
	return workflow.ExecuteActivity[string](ctx, workflow.DefaultActivityOptions, ProcessActivity, in).Get(ctx)
}

// TaskStore persists task state.
type TaskStore interface {
	SaveResult(ctx context.Context, in AgentTaskInput, result string) error
	SetStatus(ctx context.Context, id, status string) error
}

var store TaskStore

// SetStore injects the persistence used by ProcessActivity.
func SetStore(s TaskStore) { store = s }

// StartActivity marks a task in_progress so the ledger reflects that it has
// been picked up rather than still queued.
func StartActivity(ctx context.Context, in AgentTaskInput) (string, error) {
	if store != nil {
		if err := store.SetStatus(ctx, in.ID, "in_progress"); err != nil {
			return "", err
		}
	}
	return in.ID, nil
}

// ProcessActivity persists the task's terminal result.
//
// STUB: this does not run the agent. It records a placeholder so the durable
// machinery is exercised end to end. Real agent execution happens in the
// harness via its Agent tool, driven by the parent session — not here. Wiring
// orchd to spawn agent processes is a separate design decision.
func ProcessActivity(ctx context.Context, in AgentTaskInput) (string, error) {
	result := "processed: " + in.Body
	if store != nil {
		if err := store.SaveResult(ctx, in, result); err != nil {
			return "", err
		}
	}
	return result, nil
}
