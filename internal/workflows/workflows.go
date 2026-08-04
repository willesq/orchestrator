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

// AgentTask is the durable workflow for a dispatched task: pause, then process.
func AgentTask(ctx workflow.Context, in AgentTaskInput) (string, error) {
	workflow.Logger(ctx).Info("agent task started", "id", in.ID, "agent", in.Agent)
	if err := workflow.Sleep(ctx, WorkDelay); err != nil {
		return "", err
	}
	return workflow.ExecuteActivity[string](ctx, workflow.DefaultActivityOptions, ProcessActivity, in).Get(ctx)
}

// TaskStore persists the terminal result of a task.
type TaskStore interface {
	SaveResult(ctx context.Context, in AgentTaskInput, result string) error
}

var store TaskStore

// SetStore injects the persistence used by ProcessActivity.
func SetStore(s TaskStore) { store = s }

// ProcessActivity performs the (non-deterministic) work and persists the result.
func ProcessActivity(ctx context.Context, in AgentTaskInput) (string, error) {
	result := "processed: " + in.Body
	if store != nil {
		if err := store.SaveResult(ctx, in, result); err != nil {
			return "", err
		}
	}
	return result, nil
}
