package agentrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/workflows"
)

// waitFor polls cond until it holds or the test's patience runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveRunAndCancel(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"never","is_error":false}`)
	t.Setenv("FAKE_SLEEP", "30")
	dir := t.TempDir()
	done := make(chan Outcome, 1)
	go func() {
		o, err := f.runner.Run(context.Background(), Task{ID: "task-live", Agent: "dev", Body: "b", Cwd: dir})
		if err != nil {
			t.Errorf("a cancelled run returned an error: %v", err)
		}
		done <- o
	}()
	waitFor(t, "the run to report its process", func() bool {
		lr, ok := f.runner.Live("task-live")
		return ok && lr.PID > 0
	})
	lr, _ := f.runner.Live("task-live")
	if lr.SessionID != SessionID("task-live") || lr.Dir != dir || lr.Started.IsZero() {
		t.Errorf("live run = %+v", lr)
	}
	if f.runner.Cancel("nope", "") {
		t.Error("cancelled a task that is not running")
	}
	if !f.runner.Cancel("task-live", "") {
		t.Fatal("Cancel did not find the running task")
	}
	select {
	case o := <-done:
		if o.Status != StatusCancelled || o.Result != CancelledByUser {
			t.Errorf("outcome = %s %q, want cancelled by the user", o.Status, o.Result)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled run did not end")
	}
	if _, ok := f.runner.Live("task-live"); ok {
		t.Error("a finished run is still listed as live")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the task's own working directory was touched: %v", err)
	}
}

func TestCancelRemovesScratchDirectoryAndRecordsReason(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"never","is_error":false}`)
	t.Setenv("FAKE_SLEEP", "30")
	sibling := filepath.Join(f.runner.ScratchDir, "task-other")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan Outcome, 1)
	go func() {
		o, _ := f.runner.Run(context.Background(), Task{ID: "task-scratch", Agent: "dev", Body: "b"})
		done <- o
	}()
	waitFor(t, "the run to report its process", func() bool {
		lr, ok := f.runner.Live("task-scratch")
		return ok && lr.PID > 0
	})
	lr, _ := f.runner.Live("task-scratch")
	if err := os.WriteFile(filepath.Join(lr.Dir, "clone.txt"), []byte("downloaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !f.runner.Cancel("task-scratch", "cancelled: the PR head moved") {
		t.Fatal("Cancel did not find the running task")
	}
	select {
	case o := <-done:
		if o.Status != StatusCancelled || o.Result != "cancelled: the PR head moved" {
			t.Errorf("outcome = %s %q", o.Status, o.Result)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled run did not end")
	}
	if _, err := os.Stat(lr.Dir); !os.IsNotExist(err) {
		t.Errorf("scratch directory %s still there: %v", lr.Dir, err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("another task's scratch directory was removed: %v", err)
	}
}

func TestRemoveScratchStaysInsideScratchDir(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "tasks")
	keep := filepath.Join(root, "keep")
	for _, d := range []string{scratch, keep} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{ScratchDir: scratch}
	for _, id := range []string{"..", "../keep", ".", "", "a/../../keep"} {
		r.removeScratch(Task{ID: id})
	}
	r.removeScratch(Task{ID: "x", Cwd: keep})
	(&Runner{}).removeScratch(Task{ID: "keep"})
	for _, d := range []string{scratch, keep} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s removed: %v", d, err)
		}
	}
}

func TestTranscriptActivity(t *testing.T) {
	home := t.TempDir()
	if _, ok := TranscriptActivity(home, "task-x"); ok {
		t.Error("activity reported with no transcript")
	}
	if _, ok := TranscriptActivity("", "task-x"); ok {
		t.Error("activity reported with no home")
	}
	p := filepath.Join(home, ".claude", "projects", "-repo", SessionID("task-x")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	if at, ok := TranscriptActivity(home, "task-x"); !ok || !at.Equal(when) {
		t.Errorf("activity = %v %v, want %v", at, ok, when)
	}
}

// The workflow recognizes an interruption by its text, so the runner's error
// must keep carrying it.
func TestInterruptedErrCarriesTheWorkflowMarker(t *testing.T) {
	if !workflows.IsInterrupted(interruptedErr(Task{ID: "t"})) {
		t.Fatal("the workflow would not recognize the runner's interruption")
	}
}

func TestLiveRunsRefuseASecondRunAndUntrackOnlyTheirOwn(t *testing.T) {
	var l liveRuns
	first := l.track(LiveRun{TaskID: "t"}, func() {})
	if first == nil {
		t.Fatal("first run not tracked")
	}
	if l.track(LiveRun{TaskID: "t"}, func() {}) != nil {
		t.Fatal("a second live run of one task was tracked")
	}
	stale := &liveEntry{LiveRun: LiveRun{TaskID: "t"}}
	l.untrack(stale) // not the tracked entry: must not remove it
	if _, ok := l.runs["t"]; !ok {
		t.Error("untrack removed another run's entry")
	}
	l.setPID("t", 7)
	l.setPID("missing", 7)
	if l.runs["t"].PID != 7 {
		t.Error("PID not recorded")
	}
	l.untrack(first)
	if _, ok := l.runs["t"]; ok {
		t.Error("own entry not removed")
	}
}

func TestLiveByProcess(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	starts := map[int]time.Time{7: t0}
	old := procStart
	procStart = func(pid int) (time.Time, error) {
		if s, ok := starts[pid]; ok {
			return s, nil
		}
		return time.Time{}, errors.New("no such process")
	}
	t.Cleanup(func() { procStart = old })

	r := &Runner{}
	if _, ok := r.LiveByProcess(procstat.Identity{PID: 7, Started: t0}); ok {
		t.Fatal("found a run with none tracked")
	}
	r.live.track(LiveRun{TaskID: "starting"}, func() {})
	r.live.track(LiveRun{TaskID: "t"}, func() {})
	r.live.setPID("t", 7)
	if lr, ok := r.LiveByProcess(procstat.Identity{PID: 7, Started: t0}); !ok || lr.TaskID != "t" || !lr.ProcStarted.Equal(t0) {
		t.Errorf("LiveByProcess(7) = %+v, %v", lr, ok)
	}
	// PID 0 (not started), another PID, the same PID with another start time
	// (a reused PID) and an unknown start time name nothing.
	for _, id := range []procstat.Identity{{PID: 0, Started: t0}, {PID: -1, Started: t0}, {PID: 8, Started: t0},
		{PID: 7, Started: t0.Add(time.Second)}, {PID: 7}} {
		if lr, ok := r.LiveByProcess(id); ok {
			t.Errorf("LiveByProcess(%+v) = %+v", id, lr)
		}
	}
	// A run whose CLI start time could not be read matches nothing.
	r.live.track(LiveRun{TaskID: "u"}, func() {})
	r.live.setPID("u", 9)
	if lr, ok := r.LiveByProcess(procstat.Identity{PID: 9, Started: t0}); ok {
		t.Errorf("unreadable start time matched %+v", lr)
	}
}

func TestRunRefusesADuplicateSession(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"x","is_error":false}`)
	f.runner.live.track(LiveRun{TaskID: "task-d"}, func() {})
	o, err := f.runner.Run(context.Background(), Task{ID: "task-d", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil || o.Status != StatusFailed || f.ran() {
		t.Errorf("duplicate run = %+v %v, ran=%v", o, err, f.ran())
	}
}

func TestOnLiveReportsRunStartPIDAndEnd(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"done","is_error":false}`)
	var mu sync.Mutex
	var got []string
	f.runner.OnLive = func(id string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, id)
	}
	if _, err := f.runner.Run(context.Background(), Task{ID: "task-o", Agent: "dev", Body: "b", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// started, pid up, pid cleared, ended
	if len(got) != 4 {
		t.Errorf("OnLive calls = %v; want 4 for task-o", got)
	}
}
