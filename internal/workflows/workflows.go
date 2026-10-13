// Package workflows defines the durable go-workflows definitions. Workflow bodies
// must be deterministic; all non-deterministic work (I/O, persistence, running
// the agent) happens in activities. Dependencies are injected via SetStore,
// SetRunner and SetNotifier so activities stay testable and the package has no
// direct database or process import.
package workflows

import (
	"context"
	"errors"

	"github.com/cschleiden/go-workflows/workflow"
)

// AgentTaskInput is the durable input to an agent task.
type AgentTaskInput struct {
	ID    string
	Agent string
	Body  string
	// Cwd is where the agent session runs; empty runs it in a scratch directory.
	Cwd string
}

// Outcome is how a run ended: a terminal ledger status, the agent's result, and
// what the run cost.
type Outcome struct {
	Status     string
	Result     string
	CostUSD    float64
	DurationMS int64
	Turns      int
	SessionID  string
	Models     []string
	PRs        []string
}

// runOptions disables retries. A failed agent run has already had side effects
// (branches, pushes, PRs), so repeating it blindly could duplicate them; failure
// is recorded and left to a human. A run interrupted by a worker shutdown is not
// a failure: it is abandoned, handed out again after the restart, and resumes.
var runOptions = workflow.ActivityOptions{RetryOptions: workflow.RetryOptions{MaxAttempts: 1}}

// AgentTask is the durable workflow for a dispatched task: mark it running, then
// run it.
func AgentTask(ctx workflow.Context, in AgentTaskInput) (string, error) {
	workflow.Logger(ctx).Info("agent task started", "id", in.ID, "agent", in.Agent)
	if _, err := workflow.ExecuteActivity[string](
		ctx, workflow.DefaultActivityOptions, StartActivity, in).Get(ctx); err != nil {
		return "", err
	}
	return workflow.ExecuteActivity[string](ctx, runOptions, RunActivity, in).Get(ctx)
}

// TaskStore persists task state.
type TaskStore interface {
	SaveResult(ctx context.Context, in AgentTaskInput, out Outcome) error
	SetStatus(ctx context.Context, id, status string) error
}

// Runner executes a task's agent session.
type Runner interface {
	Run(ctx context.Context, in AgentTaskInput) (Outcome, error)
}

var (
	store  TaskStore
	runner Runner
	notify func(in AgentTaskInput, o Outcome)
)

// SetStore injects the persistence used by the activities.
func SetStore(s TaskStore) { store = s }

// SetRunner injects the agent runner RunActivity uses.
func SetRunner(r Runner) { runner = r }

// SetNotifier injects a callback told about every finished task.
func SetNotifier(fn func(in AgentTaskInput, o Outcome)) { notify = fn }

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

// errNoRunner means the activity ran in a worker built without a runner, which
// is a wiring bug rather than something the task did.
var errNoRunner = errors.New("no agent runner configured")

// RunActivity runs the task's agent session and records its terminal status
// and result.
func RunActivity(ctx context.Context, in AgentTaskInput) (string, error) {
	if runner == nil {
		return "", errNoRunner
	}
	out, err := runner.Run(ctx, in)
	if err != nil {
		return "", err
	}
	if store != nil {
		if err := store.SaveResult(ctx, in, out); err != nil {
			return "", err
		}
	}
	if notify != nil {
		notify(in, out)
	}
	return out.Result, nil
}
