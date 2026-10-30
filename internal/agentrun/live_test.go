package agentrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if f.runner.Cancel("nope") {
		t.Error("cancelled a task that is not running")
	}
	if !f.runner.Cancel("task-live") {
		t.Fatal("Cancel did not find the running task")
	}
	select {
	case o := <-done:
		if o.Status != StatusCancelled {
			t.Errorf("status = %s, want cancelled", o.Status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled run did not end")
	}
	if _, ok := f.runner.Live("task-live"); ok {
		t.Error("a finished run is still listed as live")
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

func TestRunRefusesADuplicateSession(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"x","is_error":false}`)
	f.runner.live.track(LiveRun{TaskID: "task-d"}, func() {})
	o, err := f.runner.Run(context.Background(), Task{ID: "task-d", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil || o.Status != StatusFailed || f.ran() {
		t.Errorf("duplicate run = %+v %v, ran=%v", o, err, f.ran())
	}
}
