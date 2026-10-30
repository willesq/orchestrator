package taskfmt

import (
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestRun(t *testing.T) {
	now := time.Unix(10_000, 0)
	if Run(&orchdv1.Task{Id: "t"}, now) != "" || RunTag(&orchdv1.Task{}) != "" {
		t.Error("an ended task should print no run")
	}
	running := Run(&orchdv1.Task{Id: "t", Run: &orchdv1.TaskRun{State: "running", Pid: 42, StartedAt: 9_400,
		LastActivity: 9_990, SessionId: "sid", Cwd: "/repo", ResumeCommand: "cd \"/repo\" && claude --resume sid"}}, now)
	for _, want := range []string{"running (pid 42, for 10m0s)", "active:  10s ago", "session: sid", "cwd:     /repo", "open it: cd"} {
		if !strings.Contains(running, want) {
			t.Errorf("running output missing %q:\n%s", want, running)
		}
	}
	if !strings.Contains(Run(&orchdv1.Task{Run: &orchdv1.TaskRun{State: "waiting"}}, now), "waiting for a worker") {
		t.Error("waiting state not described")
	}
	stalled := &orchdv1.Task{Id: "task-9", Run: &orchdv1.TaskRun{State: "stalled"}}
	if out := Run(stalled, now); !strings.Contains(out, "stalled") || !strings.Contains(out, "orch task task-9 --retry") {
		t.Errorf("stalled output = %q", out)
	}
	if RunTag(stalled) != "⚠ stalled" {
		t.Errorf("RunTag = %q", RunTag(stalled))
	}
	if since(now, now.Unix()+5) != "0s" {
		t.Error("a future time should clamp to 0s")
	}
}
