package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/spaces"
)

// A queued task runs pinned to the space its dispatch recorded (D-46,
// SP-029, INV-09). Its agent session's ID is the task's own
// (agentrun.SessionID), so orchd pins that session itself once the agent
// process starts, rather than through PinSession, which queued tasks are
// refused (callers.go). The pin gives the task's session records,
// transcript and summary the task's space (sessionPlacement), and its
// guard pin file lets the enforcing tool guard judge the task's calls in
// that space instead of refusing them as pin_missing (RC-100). The task's
// own calls still resolve to the task, never to this pin (caller_session.go).

// taskPinTimeout bounds pinning one run, which happens on the runner's
// goroutine as the agent process starts.
const taskPinTimeout = 5 * time.Second

// taskPinner pins each queued task's agent session to its task's space.
// Every field may be nil, which pins nothing.
type taskPinner struct {
	pins *pinRegistry
	// run finds the run of a task the worker is running now.
	run    func(taskID string) (agentrun.LiveRun, bool)
	spaces *spaces.Engine
	// guard writes the session's pin file for the enforcing tool guard.
	guard *guardWriter
}

// changed is told a task ID whenever the runner's view of its run changes
// (agentrun.Runner.OnLive). Once the run's agent process has started, its
// session is pinned. A failure is logged and pins nothing: PinSession
// refuses anyone else a new pin under a task's session ID
// (refuseTaskSession), so the session then has no pin, its records are
// quarantined and its tool calls refused while the guard enforces.
func (tp *taskPinner) changed(taskID string) {
	if tp == nil || tp.pins == nil || tp.spaces == nil || tp.run == nil {
		return
	}
	run, ok := tp.run(taskID)
	if !ok {
		return
	}
	res, rev, err := tp.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		// Reason: as for a session's pin, with no policy that verifies the
		// task's session can be placed nowhere but quarantine (INV-03).
		res, rev = nil, 0
	case err != nil:
		log.Printf("orchd: pinning task %s: %v", printableID(taskID), err)
		return
	case !res.Configured():
		// Reason: with no spaces nothing is pinned for a task, as before
		// (INV-10).
		return
	}
	p, ok := taskPin(run, res, rev, tp.pins.now().UTC())
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskPinTimeout)
	defer cancel()
	replaced, err := tp.pins.pinTask(ctx, p, tp.writeGuardPin)
	if err != nil {
		log.Printf("orchd: pinning task %s: %v", printableID(taskID), err)
		return
	}
	if replaced {
		log.Printf("orchd: task %s: its session ID was pinned to another space; pinned it to the task", printableID(taskID))
	}
}

// writeGuardPin writes p's guard pin file (writeGuardPinFile).
func (tp *taskPinner) writeGuardPin(p sessionPin) {
	if tp.guard != nil {
		writeGuardPinFile(tp.guard.dir, p)
	}
}

// trustedPin reports whether p may place a session's records and judge its
// tool calls. A pin under a queued task's session ID is trusted only when
// orchd took it for that task (taskPinOwner): one a harness took under the
// ID before PinSession refused that (refuseTaskSession) would otherwise
// keep the task's records in its space if pinning the task failed.
func trustedPin(p sessionPin) bool {
	return !agentrun.IsTaskSessionID(p.SessionID) || strings.HasPrefix(p.InheritedFrom, taskPinOwner(""))
}

// withPin calls fn with id's pin as it is now, while no pin can change.
func (r *pinRegistry) withPin(ctx context.Context, id string, fn func(sessionPin, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok, err := r.get(ctx, id)
	if err != nil {
		log.Printf("orchd: reading the pin of %s: %v", printableID(id), err)
		return
	}
	fn(p, ok)
}

// taskPin is the pin of run's agent session under res at revision rev
// (res nil for a policy that does not verify), and false when it gets
// none yet: its process has not started, or its session ID is not known
// (a fresh Codex run learns its thread only when it ends). A task with no
// recorded space is pinned to quarantine, so nothing it derives is placed
// in any space (INV-03).
func taskPin(run agentrun.LiveRun, res *spaces.Resolved, rev int64, now time.Time) (sessionPin, bool) {
	if !sessionIDPattern.MatchString(run.SessionID) || run.PID <= 0 || run.ProcStarted.IsZero() {
		return sessionPin{}, false
	}
	space := spaces.Quarantine
	if res != nil {
		// Reason: a space a later policy dropped, or none, is no space.
		if sp, known := res.Space(run.Space); known && sp.Kind != spaces.Public {
			space = run.Space
		}
	}
	harness := "claude"
	if run.Provider == agentrun.ProviderCodex {
		harness = "codex"
	}
	return sessionPin{
		SessionID: run.SessionID, Harness: harness,
		Process:  procstat.Identity{PID: run.PID, Started: run.ProcStarted},
		FirstCwd: run.Dir, MainCheckout: mainCheckout(run.Dir),
		Space: space, Revision: rev, PinnedAt: now,
		// Reason: marks the pin as orchd's for this task, which replay
		// trusts (replayPin); a session's own pin never names a task.
		InheritedFrom: taskPinOwner(run.TaskID),
	}, true
}

// taskPinOwner is what a task pin records as inherited_from: the task its
// space comes from.
func taskPinOwner(taskID string) string { return "task:" + taskID }

// pinTask records p, a queued task's session pin, in place of any pin its
// session ID holds, calls written with it while no other pin can change,
// and reports whether it replaced a pin in another space. Reason: the ID
// is the task's own (agentrun.SessionID, or a resumed Codex run's thread);
// a pin another harness took under it (one taken before PinSession
// refused task session IDs, or a Codex thread resumed by hand) would
// otherwise place the task's records in that harness's space. The task's
// pin taken again by the same process is left as it is.
func (r *pinRegistry) pinTask(ctx context.Context, p sessionPin, written func(sessionPin)) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, found, err := r.get(ctx, p.SessionID)
	if err != nil {
		return false, err
	}
	if found && old.Process.Same(p.Process) && old.Space == p.Space && old.InheritedFrom == p.InheritedFrom {
		written(p)
		return false, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_pins WHERE session_id = ?`, p.SessionID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_pins (`+pinColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, NULL)`,
		p.SessionID, p.Harness, p.Process.PID, p.Process.Started.UnixNano(), p.FirstCwd, p.MainCheckout,
		p.Space, p.Revision, p.PinnedAt.Format(time.RFC3339Nano), p.InheritedFrom); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	written(p)
	// Reason: an earlier run of the same task in the same space (a resume
	// after orchd restarted) is the task's own pin, not a replacement.
	return found && old.Space != p.Space, nil
}
