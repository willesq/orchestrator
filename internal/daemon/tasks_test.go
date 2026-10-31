package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cschleiden/go-workflows/backend/sqlite"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/workflows"
)

func newTaskServer(t *testing.T) (*server, *queue.SQLTaskStore, *queue.Dispatcher) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := queue.NewSQLTaskStore(conn)
	path := filepath.Join(t.TempDir(), "workflows.db")
	b := sqlite.NewSqliteBackend(path, sqlite.WithApplyMigrations(true))
	t.Cleanup(func() { _ = b.Close() })
	runs, err := queue.OpenRuns(path, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	live := &liveRunner{}
	ctl := &queue.Control{Store: store, Runs: runs, CancelLive: live.cancel}
	return &server{tasks: store, ctl: ctl, live: live, home: t.TempDir()}, store, queue.NewDispatcher(b, store)
}

func TestTaskRunStates(t *testing.T) {
	s, store, disp := newTaskServer(t)
	ctx := context.Background()
	waiting, err := disp.Dispatch(ctx, "dev", "w", "/tmp/repo", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, &orchdv1.GetTaskRequest{Id: waiting})
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetTask().GetRun(); r.GetState() != "waiting" || r.GetSessionId() != agentrun.SessionID(waiting) {
		t.Errorf("queued task run = %+v; want waiting with its session id", r)
	}

	// A row with no run at all is stalled; a transcript gives activity and a
	// resume command.
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-stuck", Agent: "dev", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(s.home, ".claude", "projects", "-repo", agentrun.SessionID("task-stuck")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(tr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTasks(ctx, &orchdv1.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var stuck *orchdv1.Task
	for _, tk := range list.GetTasks() {
		if tk.GetId() == "task-stuck" {
			stuck = tk
		}
	}
	if r := stuck.GetRun(); r.GetState() != "stalled" || r.GetLastActivity() == 0 ||
		r.GetResumeCommand() != "claude --resume "+agentrun.SessionID("task-stuck") {
		t.Errorf("stuck task run = %+v", r)
	}

	// A finished task carries no run.
	if err := store.End(ctx, "task-stuck", "failed", "x"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, &orchdv1.GetTaskRequest{Id: "task-stuck"})
	if got.GetTask().GetRun() != nil {
		t.Error("an ended task reported a run")
	}
}

func TestRetryAndCancelRPCs(t *testing.T) {
	s, store, disp := newTaskServer(t)
	ctx := context.Background()
	id, err := disp.Dispatch(ctx, "dev", "w", "", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryTask(ctx, &orchdv1.RetryTaskRequest{Id: id}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("retry while queued = %v, want FailedPrecondition", err)
	}
	resp, err := s.CancelTask(ctx, &orchdv1.CancelTaskRequest{Id: id})
	if err != nil || !strings.Contains(resp.GetOutcome(), "before it ran") {
		t.Fatalf("cancel = %v %v", resp, err)
	}
	if tk, _, _ := store.Get(ctx, id); tk.Status != queue.StatusCancelled {
		t.Errorf("status = %s", tk.Status)
	}
	if _, err := s.CancelTask(ctx, &orchdv1.CancelTaskRequest{Id: "task-none"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("cancel unknown = %v, want NotFound", err)
	}
	if _, err := s.RetryTask(ctx, &orchdv1.RetryTaskRequest{Id: "task-none"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("retry unknown = %v, want NotFound", err)
	}
	if grpcstatus.Code(taskControlError(queue.ErrQueueFull)) != codes.ResourceExhausted {
		t.Error("a full queue should map to ResourceExhausted")
	}
	if grpcstatus.Code(taskControlError(errors.New("boom"))) != codes.Internal {
		t.Error("an unexpected error should map to Internal")
	}
	empty := &server{}
	if _, err := empty.RetryTask(ctx, &orchdv1.RetryTaskRequest{Id: id}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("retry without control = %v", err)
	}
	if _, err := empty.CancelTask(ctx, &orchdv1.CancelTaskRequest{Id: id}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("cancel without control = %v", err)
	}
	if empty.taskRun(ctx, queue.Task{ID: "x", Status: queue.StatusPending}).GetState() != "stalled" {
		t.Error("with no control or runner, an unsettled task is stalled")
	}
}

func TestLiveRunnerAndHelpers(t *testing.T) {
	var nilLive *liveRunner
	if nilLive.get() != nil {
		t.Error("a nil liveRunner returned a runner")
	}
	l := &liveRunner{}
	if l.cancel("x") {
		t.Error("cancelled with no runner")
	}
	r := &agentrun.Runner{}
	l.set(r)
	if l.get() != r || l.cancel("x") {
		t.Error("liveRunner did not hold the runner, or cancelled a task it is not running")
	}
	if resumeCommand("", "") != "" || resumeCommand("/a b", "sid") != `cd '/a b' && claude --resume sid` {
		t.Errorf("resumeCommand = %q", resumeCommand("/a b", "sid"))
	}
	// A directory name any shell might read differently gets no cd at all:
	// command substitution, fish's \' escape, csh's ! history, a quote.
	for _, evil := range []string{"/tmp/x$(id)", "/tmp/x`whoami`", "/tmp/a\\'; touch /tmp/p; #", "/tmp/a!b", "/tmp/a'b", "relative/path"} {
		if got := resumeCommand(evil, "sid"); got != "claude --resume sid" {
			t.Errorf("resumeCommand(%q) = %q; want no cd", evil, got)
		}
	}
	if got := resumeCommand("/Users/me/code/my-repo_2.0", "sid"); got != "cd '/Users/me/code/my-repo_2.0' && claude --resume sid" {
		t.Errorf("a plain path should keep its cd: %q", got)
	}
	if got := resumeCommand("/tmp/a\x1b]52;c;x\x07b", "sid"); got != "claude --resume sid" {
		t.Errorf("a cwd with control characters should get no cd: %q", got)
	}
	reconcileTasks(context.Background(), nil) // a nil control is a no-op
	if userHome() == "" {
		t.Skip("no home directory")
	}
}

func TestReconcileTasksLogsAndSettles(t *testing.T) {
	s, store, _ := newTaskServer(t)
	ctx := context.Background()
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-orphan", Agent: "dev", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	// Past the grace period that protects a task whose workflow is not yet written.
	s.ctl.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	reconcileTasks(ctx, s.ctl)
	if tk, _, _ := store.Get(ctx, "task-orphan"); tk.Status != queue.StatusInterrupted {
		t.Errorf("orphan status = %s, want interrupted", tk.Status)
	}
}
