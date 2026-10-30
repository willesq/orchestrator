package agentrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatusCancelled is a task the user stopped.
const StatusCancelled = "cancelled"

// LiveRun is a task's session while this runner is running it.
type LiveRun struct {
	TaskID    string
	Provider  string
	Model     string
	SessionID string
	Dir       string
	// PID is the agent CLI's process, 0 until it has started.
	PID     int
	Started time.Time
}

type liveEntry struct {
	LiveRun
	cancel    context.CancelFunc
	cancelled bool
}

// liveRuns indexes the runs in flight, for liveness and per-task cancel.
type liveRuns struct {
	mu   sync.Mutex
	runs map[string]*liveEntry
}

// track records a run, or returns nil when the task already has one live: two
// sessions resuming the same task would duplicate its pushes and PRs.
func (l *liveRuns) track(run LiveRun, cancel context.CancelFunc) *liveEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runs == nil {
		l.runs = map[string]*liveEntry{}
	}
	if _, busy := l.runs[run.TaskID]; busy {
		return nil
	}
	e := &liveEntry{LiveRun: run, cancel: cancel}
	l.runs[run.TaskID] = e
	return e
}

// untrack removes e, and only e, from the index.
func (l *liveRuns) untrack(e *liveEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runs[e.TaskID] == e {
		delete(l.runs, e.TaskID)
	}
}

func (l *liveRuns) setPID(id string, pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.runs[id]; e != nil {
		e.PID = pid
	}
}

func (l *liveRuns) wasCancelled(e *liveEntry) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return e.cancelled
}

// Live reports the task's run when this runner is running it now.
func (r *Runner) Live(taskID string) (LiveRun, bool) {
	r.live.mu.Lock()
	defer r.live.mu.Unlock()
	e, ok := r.live.runs[taskID]
	if !ok {
		return LiveRun{}, false
	}
	return e.LiveRun, true
}

// Cancel stops the task's run if this runner is running it, reporting whether
// it was. The run then ends with StatusCancelled rather than as a failure.
func (r *Runner) Cancel(taskID string) bool {
	r.live.mu.Lock()
	defer r.live.mu.Unlock()
	e, ok := r.live.runs[taskID]
	if !ok {
		return false
	}
	e.cancelled = true
	e.cancel()
	return true
}

// TranscriptActivity is when the task's Claude Code session transcript under
// home last changed, the best sign of whether a run is doing anything.
func TranscriptActivity(home, taskID string) (time.Time, bool) {
	if home == "" {
		return time.Time{}, false
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", SessionID(taskID)+".jsonl"))
	var newest time.Time
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, !newest.IsZero()
}

// pidKey carries the callback runCLI reports the started process to.
type pidKey struct{}

func withPIDReport(ctx context.Context, report func(int)) context.Context {
	return context.WithValue(ctx, pidKey{}, report)
}

func reportPID(ctx context.Context, pid int) {
	if f, ok := ctx.Value(pidKey{}).(func(int)); ok {
		f(pid)
	}
}
