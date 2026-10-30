package queue

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/willesq/orchestrator/internal/workflows"
)

// controlHarness is a ledger plus a real go-workflows SQLite backend, so the
// run index's read of the backend's own tables is checked against the pinned
// library version.
type controlHarness struct {
	t     *testing.T
	store *SQLTaskStore
	b     backend.Backend
	runs  *Runs
	ctl   *Control
}

func newControlHarness(t *testing.T) *controlHarness {
	t.Helper()
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "workflows.db")
	b := sqlite.NewSqliteBackend(path, sqlite.WithApplyMigrations(true))
	t.Cleanup(func() { _ = b.Close() })
	runs, err := OpenRuns(path, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	// The clock runs past reconcileGrace, so tasks the test just created count.
	ctl := &Control{Store: store, Runs: runs, Now: func() time.Time { return time.Now().Add(2 * time.Minute) }}
	return &controlHarness{t: t, store: store, b: b, runs: runs, ctl: ctl}
}

// legacyTask stands in for a workflow run that ended the way 0.28.0's did when
// orchd restarted mid-run: failed with the runner's interruption error.
func legacyTask(_ workflow.Context, in workflows.AgentTaskInput) (string, error) {
	if strings.Contains(in.Body, "fail") {
		return "", errors.New("store unavailable")
	}
	if strings.Contains(in.Body, "ok") {
		return "done", nil
	}
	return "", errors.New("task " + in.ID + " " + workflows.InterruptedMarker + ": context canceled")
}

// finishedRun creates a ledger row and a run of legacyTask for it, and waits
// for the run to finish.
func (h *controlHarness) finishedRun(id, body string) workflows.AgentTaskInput {
	h.t.Helper()
	ctx := context.Background()
	in := taskInput(id, "dev", body, "/tmp/repo", map[string]string{"/tmp/repo": "abc123"}, workflows.Route{Model: "sonnet"})
	if err := h.store.Create(ctx, in); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetStatus(ctx, id, StatusInProgress); err != nil {
		h.t.Fatal(err)
	}
	h.finishInstance(id, in)
	return in
}

// finishInstance runs a legacyTask instance named instanceID to completion.
func (h *controlHarness) finishInstance(instanceID string, in workflows.AgentTaskInput) {
	h.t.Helper()
	ctx := context.Background()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := worker.New(h.b, nil)
	if err := w.RegisterWorkflow(legacyTask); err != nil {
		h.t.Fatal(err)
	}
	if err := w.Start(wctx); err != nil {
		h.t.Fatal(err)
	}
	c := client.New(h.b)
	inst, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: instanceID}, legacyTask, in)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := c.WaitForWorkflowInstance(ctx, inst, 10*time.Second); err != nil {
		h.t.Fatal(err)
	}
	cancel()
	_ = w.WaitForCompletion()
}

func (h *controlHarness) status(id string) (string, string) {
	h.t.Helper()
	tk, found, err := h.store.Get(context.Background(), id)
	if err != nil || !found {
		h.t.Fatalf("get %s: %v %v", id, found, err)
	}
	return tk.Status, tk.Result
}

func TestReconcileResumesSettlesAndMarks(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	interrupted := h.finishedRun("task-int", "interrupted body")
	h.finishedRun("task-fail", "fail body")
	h.finishedRun("task-done", "ok body")
	// A row with no workflow run at all.
	if err := h.store.Create(ctx, task("task-orphan", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	// A pending task whose run is still waiting for a worker is left alone.
	disp := NewDispatcher(h.b, h.store)
	waiting, err := disp.Dispatch(ctx, "dev", "waiting", "", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}

	resumed, settled, err := h.ctl.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed != 1 || settled != 3 {
		t.Errorf("resumed %d settled %d; want 1 and 3", resumed, settled)
	}
	if s, _ := h.status("task-int"); s != StatusPending {
		t.Errorf("interrupted task status = %s, want pending (resumed)", s)
	}
	run, ok, err := h.runs.Latest(ctx, "task-int")
	if err != nil || !ok || !run.Active || !strings.HasPrefix(run.Instance.InstanceID, "task-int#retry-") {
		t.Fatalf("latest run of task-int = %+v %v %v; want an active retry", run, ok, err)
	}
	in, err := h.runs.Input(ctx, "task-int")
	if err != nil || in.Body != interrupted.Body || in.Cwd != "/tmp/repo" || in.Heads["/tmp/repo"] != "abc123" || in.Route.Model != "sonnet" {
		t.Errorf("resumed input = %+v, %v; want the original", in, err)
	}
	if s, r := h.status("task-fail"); s != "failed" || !strings.Contains(r, "store unavailable") {
		t.Errorf("failed workflow = %s %q", s, r)
	}
	for _, id := range []string{"task-done", "task-orphan"} {
		if s, r := h.status(id); s != StatusInterrupted || !strings.Contains(r, "--retry") {
			t.Errorf("%s = %s %q; want interrupted with how to retry", id, s, r)
		}
	}
	if s, _ := h.status(waiting); s != StatusPending {
		t.Errorf("a task waiting for a worker was touched: %s", s)
	}
	// A second pass changes nothing: the resumed run is active now.
	if r, s, err := h.ctl.Reconcile(ctx); err != nil || r != 0 || s != 0 {
		t.Errorf("second reconcile = %d %d %v", r, s, err)
	}
}

func TestRetryAndCancel(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	h.finishedRun("task-a", "ok body")
	if err := h.store.End(ctx, "task-a", "failed", "tests broke"); err != nil {
		t.Fatal(err)
	}
	next, err := h.ctl.Retry(ctx, "task-a")
	if err != nil || !strings.HasPrefix(next, "task-a#retry-") {
		t.Fatalf("retry = %q, %v", next, err)
	}
	if s, _ := h.status("task-a"); s != StatusPending {
		t.Errorf("retried status = %s", s)
	}
	if _, err := h.ctl.Retry(ctx, "task-a"); !errors.Is(err, ErrState) {
		t.Errorf("retrying while the retry is active = %v, want ErrState", err)
	}
	// Cancel before any worker ran it: the waiting run is cancelled.
	msg, err := h.ctl.Cancel(ctx, "task-a")
	if err != nil || !strings.Contains(msg, "before it ran") {
		t.Fatalf("cancel = %q, %v", msg, err)
	}
	if s, _ := h.status("task-a"); s != StatusCancelled {
		t.Errorf("cancelled status = %s", s)
	}
	if _, err := h.ctl.Cancel(ctx, "task-a"); !errors.Is(err, ErrState) {
		t.Errorf("cancelling an ended task = %v, want ErrState", err)
	}

	// A run live in this process is stopped through CancelLive.
	if err := h.store.Create(ctx, task("task-live", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	h.ctl.CancelLive = func(id string) bool { return id == "task-live" }
	if msg, err := h.ctl.Cancel(ctx, "task-live"); err != nil || !strings.Contains(msg, "running session") {
		t.Errorf("live cancel = %q, %v", msg, err)
	}

	for _, id := range []string{"task-missing"} {
		if _, err := h.ctl.Retry(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("retry unknown = %v", err)
		}
		if _, err := h.ctl.Cancel(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("cancel unknown = %v", err)
		}
	}
	if err := h.store.Create(ctx, task("task-norun", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctl.Retry(ctx, "task-norun"); !errors.Is(err, ErrState) {
		t.Errorf("retry with no recorded run = %v, want ErrState", err)
	}
}

func TestUnsettled(t *testing.T) {
	for s, want := range map[string]bool{StatusPending: true, StatusInProgress: true, "complete": false, StatusCancelled: false, StatusInterrupted: false} {
		if Unsettled(s) != want {
			t.Errorf("Unsettled(%s) = %v", s, !want)
		}
	}
}

// blockingTask waits until cancelled, standing in for a run the user cancels.
func blockingTask(ctx workflow.Context, _ workflows.AgentTaskInput) (string, error) {
	// A durable sleep ends with an error when the workflow is cancelled.
	return "", workflow.Sleep(ctx, time.Hour)
}

func TestRunsEdges(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	if _, ok, err := h.runs.Latest(ctx, "task-none"); ok || err != nil {
		t.Errorf("Latest(unknown) = %v %v", ok, err)
	}
	if _, err := h.runs.Input(ctx, "task-none"); err == nil {
		t.Error("Input with no runs should fail")
	}

	// A run no worker has picked up: active, no failure, no recorded input.
	c := client.New(h.b)
	if _, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: "task-q"}, workflows.AgentTask,
		workflows.AgentTaskInput{ID: "task-q"}); err != nil {
		t.Fatal(err)
	}
	run, ok, err := h.runs.Latest(ctx, "task-q")
	if err != nil || !ok || !run.Active {
		t.Fatalf("queued run = %+v %v %v", run, ok, err)
	}
	if f, err := h.runs.Failure(ctx, run); f != "" || err != nil {
		t.Errorf("Failure(active) = %q %v", f, err)
	}
	if _, err := h.runs.Input(ctx, "task-q"); err == nil || !strings.Contains(err.Error(), "no run of task-q") {
		t.Errorf("Input(never started) = %v", err)
	}

	// A run cancelled while running reports "canceled".
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := worker.New(h.b, nil)
	if err := w.RegisterWorkflow(blockingTask); err != nil {
		t.Fatal(err)
	}
	if err := w.Start(wctx); err != nil {
		t.Fatal(err)
	}
	inst, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: "task-c"}, blockingTask,
		workflows.AgentTaskInput{ID: "task-c"})
	if err != nil {
		t.Fatal(err)
	}
	run, _, _ = h.runs.Latest(ctx, "task-c")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if in, err := h.runs.Input(ctx, "task-c"); err == nil && in.ID == "task-c" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the blocking run never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.runs.Cancel(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitForWorkflowInstance(ctx, inst, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = w.WaitForCompletion()
	run, _, _ = h.runs.Latest(ctx, "task-c")
	if f, err := h.runs.Failure(ctx, run); err != nil || f == "" {
		t.Errorf("Failure(cancelled) = %q %v; want a reason", f, err)
	}
}

func TestRunsReportStoreErrors(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	if err := h.store.Create(ctx, task("task-e", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	if err := h.runs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.runs.Latest(ctx, "task-e"); err == nil {
		t.Error("Latest on a closed index did not fail")
	}
	if _, err := h.runs.Input(ctx, "task-e"); err == nil {
		t.Error("Input on a closed index did not fail")
	}
	if _, _, err := h.ctl.Reconcile(ctx); err == nil || !strings.Contains(err.Error(), "task-e") {
		t.Errorf("Reconcile should report the task it could not check: %v", err)
	}
	if _, err := h.ctl.Retry(ctx, "task-e"); err == nil {
		t.Error("Retry on a closed index did not fail")
	}
	if _, err := h.ctl.Cancel(ctx, "task-e"); err == nil {
		t.Error("Cancel on a closed index did not fail")
	}
}

func TestReconcileGuards(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()

	// A task dispatched just now is left alone: its workflow may not exist yet.
	if err := h.store.Create(ctx, task("task-new", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	h.ctl.Now = time.Now
	if r, s, err := h.ctl.Reconcile(ctx); r+s != 0 || err != nil {
		t.Errorf("a brand-new task was reconciled: %d %d %v", r, s, err)
	}

	// A task queued over a day ago that never ran is stopped, not run.
	disp := NewDispatcher(h.b, h.store)
	stale, err := disp.Dispatch(ctx, "dev", "old work", "", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}
	h.ctl.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if _, _, err := h.ctl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, r := h.status(stale); s != StatusInterrupted || !strings.Contains(r, "over a day ago") {
		t.Errorf("stale task = %s %q", s, r)
	}

	// After maxAutoRetries interrupted retries, Reconcile stops resuming.
	in := h.finishedRun("task-loop", "interrupted body")
	for i := 0; i < maxAutoRetries; i++ {
		h.finishInstance("task-loop#retry-"+string(rune('a'+i)), in)
	}
	if n, err := h.runs.Retries(ctx, "task-loop"); err != nil || n != maxAutoRetries {
		t.Fatalf("Retries = %d %v", n, err)
	}
	if r, _, err := h.ctl.Reconcile(ctx); err != nil || r != 0 {
		t.Errorf("resumed %d after the retry cap: %v", r, err)
	}
	if s, r := h.status("task-loop"); s != StatusInterrupted || !strings.Contains(r, "automatic retries") {
		t.Errorf("capped task = %s %q", s, r)
	}
}

func TestRetryHonorsMaxActive(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	h.finishedRun("task-r", "ok body")
	if err := h.store.End(ctx, "task-r", "failed", "x"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Create(ctx, task("task-busy", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	h.ctl.MaxActive = 1
	if _, err := h.ctl.Retry(ctx, "task-r"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("retry past max_active = %v, want ErrQueueFull", err)
	}
}

func TestCancelledIsFinal(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	in := task("task-f", "dev", "x")
	if err := h.store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := h.store.End(ctx, "task-f", StatusCancelled, "cancelled by the user"); err != nil {
		t.Fatal(err)
	}
	// A run that raced the cancel cannot revive or overwrite it.
	if err := h.store.SetStatus(ctx, "task-f", StatusInProgress); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveResult(ctx, in, workflows.Outcome{Status: "complete", Result: "done"}); err != nil {
		t.Fatal(err)
	}
	if s, r := h.status("task-f"); s != StatusCancelled || r != "cancelled by the user" {
		t.Errorf("cancelled row became %s %q", s, r)
	}
	if s, ok, err := h.store.Status(ctx, "task-f"); s != StatusCancelled || !ok || err != nil {
		t.Errorf("Status = %s %v %v", s, ok, err)
	}
	if _, ok, err := h.store.Status(ctx, "task-none"); ok || err != nil {
		t.Errorf("Status(unknown) = %v %v", ok, err)
	}
	// Cancel stops a session that started just as the row was written.
	if err := h.store.Create(ctx, task("task-g", "dev", "x")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h.ctl.CancelLive = func(string) bool { calls++; return calls > 1 }
	if msg, err := h.ctl.Cancel(ctx, "task-g"); err != nil || !strings.Contains(msg, "just started") {
		t.Errorf("cancel with a late-starting session = %q %v", msg, err)
	}
}
