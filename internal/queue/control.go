package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the run index's read-only handle on workflows.db

	"github.com/willesq/orchestrator/internal/workflows"
)

// Task statuses beyond the runner's own (complete, failed, blocked).
const (
	StatusPending     = "pending"
	StatusInProgress  = "in_progress"
	StatusInterrupted = "interrupted"
	StatusCancelled   = "cancelled"
)

// Unsettled reports whether a ledger status still waits on a run.
func Unsettled(status string) bool { return status == StatusPending || status == StatusInProgress }

// End records a terminal status and reason for a task whose run ended outside
// a normal outcome (reconciled, cancelled before it ran), leaving its metrics.
func (s *SQLTaskStore) End(ctx context.Context, id, status, result string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status=?, result=?, updated_at=datetime('now') WHERE id=?`, status, result, id)
	return err
}

// unsettledTask is a task the ledger still shows pending or in progress.
type unsettledTask struct {
	id, status string
	created    time.Time
}

// unsettled lists the tasks the ledger still shows pending or in progress,
// with when each was dispatched (rows from before created_at use their last
// update).
func (s *SQLTaskStore) unsettled(ctx context.Context) ([]unsettledTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, status, COALESCE(created_at, unixepoch(updated_at), 0) FROM tasks WHERE status IN (?, ?)`,
		StatusPending, StatusInProgress)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []unsettledTask
	for rows.Next() {
		var t unsettledTask
		var created int64
		if err := rows.Scan(&t.id, &t.status, &created); err != nil {
			return nil, err
		}
		t.created = time.Unix(created, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Control resumes, retries and cancels tasks, keeping the ledger and the
// workflow runs in step.
type Control struct {
	Store *SQLTaskStore
	Runs  *Runs
	// CancelLive stops a task this process is running now, reporting whether
	// it was; nil means no run is ever live here.
	CancelLive func(taskID string) bool
	// MaxActive caps pending plus running tasks for a retry, as for a
	// dispatch; 0 means no cap.
	MaxActive int
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// mu serializes Reconcile, Retry and Cancel: each checks a task's runs
	// and then acts, and two racing on one task could start two runs of the
	// same session, duplicating its pushes and PRs.
	mu sync.Mutex
}

// Reconcile's limits.
const (
	// reconcileGrace leaves alone a task dispatched this recently: its row is
	// written before its workflow, so a reconcile in between would find no run.
	reconcileGrace = time.Minute
	// staleAfter is how old a task that never ran may be before Reconcile
	// stops it instead of letting a worker start it against today's repos
	// (one dispatched while no worker ran, say).
	staleAfter = 24 * time.Hour
	// maxAutoRetries bounds the retry runs Reconcile starts on its own, so a
	// worker that keeps crashing cannot rerun a task without end.
	maxAutoRetries = 3
)

func (c *Control) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ErrNotFound is a task the ledger has no row for.
var ErrNotFound = errors.New("no such task")

// ErrState is an action the task's current state does not allow.
var ErrState = errors.New("not allowed in the task's state")

// Reconcile settles every task the ledger shows pending or in progress that no
// active workflow run owns: one cut off by a worker shutdown is resumed, one
// whose workflow failed is recorded failed, and one with no run at all is
// marked interrupted with how to retry it. It never leaves a task waiting on a
// run that does not exist (issue #100).
func (c *Control) Reconcile(ctx context.Context) (resumed, settled int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tasks, err := c.Store.unsettled(ctx)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, t := range tasks {
		if c.now().Sub(t.created) < reconcileGrace {
			continue
		}
		r, s, err := c.reconcileOne(ctx, t)
		resumed, settled = resumed+r, settled+s
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.id, err))
		}
	}
	return resumed, settled, errors.Join(errs...)
}

func (c *Control) reconcileOne(ctx context.Context, t unsettledTask) (resumed, settled int, err error) {
	id := t.id
	run, ok, err := c.Runs.Latest(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		log.Printf("tasks: %s has no workflow run; marking it interrupted", id)
		return 0, 1, c.Store.End(ctx, id, StatusInterrupted,
			"no workflow run owns this task; run `orch task "+id+" --retry` to run it again")
	}
	if run.Active {
		if t.status != StatusPending || c.now().Sub(t.created) < staleAfter {
			return 0, 0, nil
		}
		started, err := c.Runs.Started(ctx, run)
		if err != nil || started {
			return 0, 0, err
		}
		log.Printf("tasks: %s was dispatched %s ago and never ran; stopping it rather than running it now", id,
			c.now().Sub(t.created).Round(time.Hour))
		if err := c.Runs.Cancel(ctx, run); err != nil {
			return 0, 0, err
		}
		return 0, 1, c.Store.End(ctx, id, StatusInterrupted,
			"dispatched over a day ago and never run (no worker was running); run `orch task "+id+
				" --retry` if it is still wanted")
	}
	failure, err := c.Runs.Failure(ctx, run)
	if err != nil {
		return 0, 0, err
	}
	retries := 0
	if workflows.IsInterrupted(errors.New(failure)) {
		if retries, err = c.Runs.Retries(ctx, id); err != nil {
			return 0, 0, err
		}
	}
	switch {
	case workflows.IsInterrupted(errors.New(failure)) && retries >= maxAutoRetries:
		log.Printf("tasks: %s was interrupted again after %d automatic retries; leaving it to the user", id, retries)
		return 0, 1, c.Store.End(ctx, id, StatusInterrupted,
			fmt.Sprintf("interrupted again after %d automatic retries; run `orch task %s --retry` to try once more", retries, id))
	case workflows.IsInterrupted(errors.New(failure)):
		in, err := c.Runs.Input(ctx, id)
		if err != nil {
			return 0, 0, err
		}
		next, err := c.Runs.Start(ctx, in)
		if err != nil {
			return 0, 0, err
		}
		log.Printf("tasks: %s was interrupted by a restart; resuming it as %s", id, next)
		return 1, 0, c.Store.SetStatus(ctx, id, StatusPending)
	case failure != "":
		log.Printf("tasks: %s's workflow failed without recording a result; marking it failed", id)
		return 0, 1, c.Store.End(ctx, id, "failed", "the task's workflow failed: "+failure)
	default:
		log.Printf("tasks: %s's workflow finished without recording a result; marking it interrupted", id)
		return 0, 1, c.Store.End(ctx, id, StatusInterrupted,
			"the run ended without recording a result; run `orch task "+id+" --retry` to run it again")
	}
}

// Retry runs a settled task again, or one stranded unsettled with no active
// run. The runner resumes the task's session when one exists, so a retry
// continues the earlier work rather than starting over.
func (c *Control) Retry(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, found, err := c.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if c.MaxActive > 0 && !Unsettled(t.Status) {
		active, err := c.Store.ActiveCount(ctx)
		if err != nil {
			return "", err
		}
		if active >= c.MaxActive {
			return "", fmt.Errorf("%w: %d tasks pending or running (queue.max_active=%d)", ErrQueueFull, active, c.MaxActive)
		}
	}
	run, ok, err := c.Runs.Latest(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s has no recorded run to take its input from", ErrState, id)
	}
	if run.Active {
		return "", fmt.Errorf("%w: %s is %s and its run is still active", ErrState, id, t.Status)
	}
	in, err := c.Runs.Input(ctx, id)
	if err != nil {
		return "", err
	}
	next, err := c.Runs.Start(ctx, in)
	if err != nil {
		return "", err
	}
	log.Printf("tasks: %s retried as %s", id, next)
	return next, c.Store.SetStatus(ctx, id, StatusPending)
}

// Cancel stops a task: a run in progress here is stopped (it records itself
// cancelled), a run still waiting for a worker is cancelled, and a task with
// no active run is marked cancelled.
func (c *Control) Cancel(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, found, err := c.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !Unsettled(t.Status) {
		return "", fmt.Errorf("%w: %s already ended (%s)", ErrState, id, t.Status)
	}
	if c.CancelLive != nil && c.CancelLive(id) {
		log.Printf("tasks: %s cancelled while running", id)
		return "stopping its running session", nil
	}
	run, ok, err := c.Runs.Latest(ctx, id)
	if err != nil {
		return "", err
	}
	if ok && run.Active {
		if err := c.Runs.Cancel(ctx, run); err != nil {
			return "", err
		}
	}
	log.Printf("tasks: %s cancelled before it ran", id)
	if err := c.Store.End(ctx, id, StatusCancelled, "cancelled by the user"); err != nil {
		return "", err
	}
	// Reason: a worker may have started the session between the checks above
	// and the ledger write; stop it too. The row is final either way.
	if c.CancelLive != nil && c.CancelLive(id) {
		return "stopping its session, which had just started", nil
	}
	return "cancelled before it ran", nil
}
