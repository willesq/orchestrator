package queue

import (
	"context"
	"testing"

	"github.com/willesq/orchestrator/internal/workflows"
)

// TestRecordedInputOfAWaitingTask: a task no worker has picked up keeps its
// input, and so the space it recorded at dispatch, in the backend's pending
// queue; RecordedInput reads it there, as Input reads a started run's
// history (SP-026).
func TestRecordedInputOfAWaitingTask(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	d := NewDispatcher(h.b, h.store)
	id, err := d.Dispatch(WithReadSpace(ctx, "acme"), "dev", "waiting", "/tmp/repo", workflows.Route{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.runs.Input(ctx, id); err == nil {
		t.Fatal("Input read a run no worker started")
	}
	in, err := h.runs.RecordedInput(ctx, id)
	if err != nil || in.Space != "acme" || in.Body != "waiting" {
		t.Fatalf("RecordedInput = %+v, %v", in, err)
	}
	// A started run's input still comes from its history.
	done := h.finishedRun("task-done", "ok")
	if in, err := h.runs.RecordedInput(ctx, "task-done"); err != nil || in.Body != done.Body {
		t.Fatalf("RecordedInput of a finished run = %+v, %v", in, err)
	}
	if _, err := h.runs.RecordedInput(ctx, "task-none"); err == nil {
		t.Error("a task with no run has an input")
	}
}
