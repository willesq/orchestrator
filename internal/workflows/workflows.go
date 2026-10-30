// Package workflows defines the durable go-workflows definitions. Workflow bodies
// must be deterministic; all non-deterministic work (I/O, persistence, running
// the agent) happens in activities. Dependencies are injected via SetStore,
// SetRunner and SetNotifier so activities stay testable and the package has no
// direct database or process import.
package workflows

import (
	"context"
	"errors"
	"strings"

	"github.com/cschleiden/go-workflows/workflow"
)

// AgentTaskInput is the durable input to an agent task.
type AgentTaskInput struct {
	ID    string
	Agent string
	Body  string
	// Cwd is where the agent session runs; empty runs it in a scratch directory.
	Cwd string
	// Heads is the HEAD of each checkout of Cwd's repository at dispatch, kept in
	// the durable input so a task resumed after a restart still knows what it
	// started from.
	Heads map[string]string
	// Route is the model the task was routed to at dispatch. It is fixed then,
	// so a task resumed after a restart keeps running on the same model.
	Route Route
}

// Route names the model a task runs on. Empty fields leave the choice to the
// agent's own configuration.
type Route struct {
	Kind     string // task kind the route was chosen for
	Provider string
	Model    string
	Effort   string
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
	// Verification is how the work was checked before completion.
	Verification string
}

// runOptions disables retries. A failed agent run has already had side effects
// (branches, pushes, PRs), so repeating it blindly could duplicate them; failure
// is recorded and left to a human. An interrupted run is not a failure; AgentTask
// runs it again, and the runner resumes its session.
var runOptions = workflow.ActivityOptions{RetryOptions: workflow.RetryOptions{MaxAttempts: 1}}

// InterruptedMarker is in the error of a run cut off by a worker shutdown. It
// is matched by text because an activity's error reaches the workflow
// serialized, without its Go type.
const InterruptedMarker = "interrupted by worker shutdown"

// IsInterrupted reports whether err is a run cut off by a worker shutdown.
func IsInterrupted(err error) bool {
	return err != nil && strings.Contains(err.Error(), InterruptedMarker)
}

// maxResumes bounds how often one workflow runs an interrupted task again, so
// a worker that keeps restarting cannot loop a task forever.
const maxResumes = 20

// AgentTask is the durable workflow for a dispatched task: mark it running, then
// run it, running it again whenever a worker shutdown interrupted it.
//
// Reason: go-workflows records any error an activity returns, even one from a
// stopping worker, as that activity's failure; with retries off, the workflow
// then failed and the ledger kept the task in_progress with nothing running it
// (issue #100). Catching the interruption here makes the next worker (after the
// restart) run it again, and the runner resumes the same session.
func AgentTask(ctx workflow.Context, in AgentTaskInput) (string, error) {
	workflow.Logger(ctx).Info("agent task started", "id", in.ID, "agent", in.Agent)
	if _, err := workflow.ExecuteActivity[string](
		ctx, workflow.DefaultActivityOptions, StartActivity, in).Get(ctx); err != nil {
		return "", err
	}
	for resumes := 0; ; resumes++ {
		result, err := workflow.ExecuteActivity[string](ctx, runOptions, RunActivity, in).Get(ctx)
		if !IsInterrupted(err) || resumes >= maxResumes {
			return result, err
		}
		workflow.Logger(ctx).Info("agent task interrupted by a worker shutdown; resuming", "id", in.ID, "resume", resumes+1)
	}
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

// statusReader is a TaskStore that can also read a task's status.
type statusReader interface {
	Status(ctx context.Context, id string) (string, bool, error)
}

// RunActivity runs the task's agent session and records its terminal status
// and result. A task cancelled before its session started is not run.
func RunActivity(ctx context.Context, in AgentTaskInput) (string, error) {
	if runner == nil {
		return "", errNoRunner
	}
	if sr, ok := store.(statusReader); ok {
		if status, found, err := sr.Status(ctx, in.ID); err == nil && found && status == "cancelled" {
			return "cancelled by the user", nil
		}
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
