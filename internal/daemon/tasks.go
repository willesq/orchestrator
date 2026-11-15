package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// liveRunner is the worker's current runner. The worker unit builds a new one
// on every start, so the task RPCs read it here rather than holding their own.
type liveRunner struct {
	mu sync.Mutex
	r  *agentrun.Runner
}

func (l *liveRunner) set(r *agentrun.Runner) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.r = r
}

func (l *liveRunner) get() *agentrun.Runner {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r
}

// taskOf returns the run the current runner is running in agent process
// proc.
func (l *liveRunner) taskOf(proc procstat.Identity) (agentrun.LiveRun, bool) {
	r := l.get()
	if r == nil {
		return agentrun.LiveRun{}, false
	}
	return r.LiveByProcess(proc)
}

// run returns the current runner's run of task id, if it is running it.
func (l *liveRunner) run(id string) (agentrun.LiveRun, bool) {
	r := l.get()
	if r == nil {
		return agentrun.LiveRun{}, false
	}
	return r.Live(id)
}

// cancel stops a task the current runner is running, with reason as its
// result, reporting whether it was.
func (l *liveRunner) cancel(taskID, reason string) bool {
	r := l.get()
	return r != nil && r.Cancel(taskID, reason)
}

// reconcileTasks settles the tasks a stopped worker left behind; the worker
// unit runs it each time it starts.
func reconcileTasks(ctx context.Context, ctl *queue.Control) {
	if ctl == nil {
		return
	}
	resumed, settled, err := ctl.Reconcile(ctx)
	if resumed+settled > 0 {
		log.Printf("tasks: after the worker started, resumed %d and settled %d task(s) a stopped worker left behind", resumed, settled)
	}
	if err != nil {
		log.Printf("tasks: reconciling tasks after start: %v", err)
	}
}

// taskRun says whether anything is working on an unsettled task: its live
// session here, a workflow run queued for a worker, or nothing (stalled).
func (s *server) taskRun(ctx context.Context, t queue.Task) *orchdv1.TaskRun {
	if !queue.Unsettled(t.Status) {
		return nil
	}
	run := &orchdv1.TaskRun{State: "stalled"}
	if t.Provider != agentrun.ProviderCodex {
		run.SessionId = agentrun.SessionID(t.ID)
	}
	if r := s.live.get(); r != nil {
		if lr, ok := r.Live(t.ID); ok {
			run.State, run.Pid, run.StartedAt, run.Cwd = "running", int64(lr.PID), lr.Started.Unix(), lr.Dir
			if lr.SessionID != "" {
				run.SessionId = lr.SessionID
			}
		}
	}
	if run.State != "running" && s.ctl != nil {
		if latest, ok, err := s.ctl.Runs.Latest(ctx, t.ID); err == nil && ok && latest.Active {
			run.State = "waiting"
		}
	}
	if run.Cwd == "" && s.ctl != nil {
		if in, err := s.ctl.Runs.Input(ctx, t.ID); err == nil {
			run.Cwd = in.Cwd
		}
	}
	if at, ok := agentrun.TranscriptActivity(s.home, t.ID); ok {
		run.LastActivity = at.Unix()
		if t.Provider != agentrun.ProviderCodex {
			run.ResumeCommand = resumeCommand(run.Cwd, run.SessionId)
		}
	}
	return run
}

// resumeCommand opens a task's Claude Code session in the user's terminal.
//
// Reason: the user pastes this into whatever shell they use, and the cwd came
// from a dispatch an agent may have made. Single quotes are inert in POSIX sh,
// but fish reads \' and \\ inside them and csh expands ! there, so the cd is
// included only for a path of plainly safe characters; any other path is
// left out (the cwd is shown separately) rather than quoted cleverly.
func resumeCommand(cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	cmd := "claude --resume " + sessionID
	if !safePath.MatchString(cwd) {
		return cmd
	}
	return "cd " + shellQuote(cwd) + " && " + cmd
}

// safePath is a path every common shell reads the same inside single quotes.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9 ._/+@:,=~-]*$`)

// shellQuote quotes s for POSIX sh: inside single quotes nothing expands, and
// a single quote is closed, escaped and reopened.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_RetryTask_FullMethodName, "retry tasks",
	spaceDecision{spaceNone, "retries a task of the caller's own space (SP-026, RC-708)"}))

func (s *server) RetryTask(ctx context.Context, req *orchdv1.RetryTaskRequest) (*orchdv1.RetryTaskResponse, error) {
	if s.ctl == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task control unavailable")
	}
	// Reason: a retry reruns the task with the space it recorded, reading
	// that space's memory, so only its own space reruns it (RC-708).
	if !s.changesTask(ctx, s.recordScope(ctx, orchdv1.Orchd_RetryTask_FullMethodName), req.GetId()) {
		return nil, errTaskNotFound(req.GetId())
	}
	id, err := s.ctl.Retry(ctx, req.GetId())
	if err != nil {
		return nil, taskControlError(err)
	}
	return &orchdv1.RetryTaskResponse{RunId: id}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_CancelTask_FullMethodName, "cancel tasks",
	spaceDecision{spaceNone, "cancels a task of the caller's own space (SP-026)"}))

func (s *server) CancelTask(ctx context.Context, req *orchdv1.CancelTaskRequest) (*orchdv1.CancelTaskResponse, error) {
	if s.ctl == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task control unavailable")
	}
	if !s.changesTask(ctx, s.recordScope(ctx, orchdv1.Orchd_CancelTask_FullMethodName), req.GetId()) {
		return nil, errTaskNotFound(req.GetId())
	}
	outcome, err := s.ctl.Cancel(ctx, req.GetId())
	if err != nil {
		return nil, taskControlError(err)
	}
	return &orchdv1.CancelTaskResponse{Outcome: outcome}, nil
}

// taskControlError maps retry and cancel errors to RPC codes.
func taskControlError(err error) error {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		return grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, queue.ErrState):
		return grpcstatus.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, queue.ErrQueueFull):
		return grpcstatus.Error(codes.ResourceExhausted, err.Error())
	}
	return grpcstatus.Error(codes.Internal, err.Error())
}

// userHome is the home transcripts live under, or "".
func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}
