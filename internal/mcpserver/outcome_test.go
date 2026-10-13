package mcpserver

import (
	"context"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestQueueReadShowsOutcome(t *testing.T) {
	qt := queueTools{client: &fakeClient{getTask: &orchdv1.GetTaskResponse{Found: true, Task: &orchdv1.Task{
		Id: "t1", Status: "complete", Agent: "dev", Body: "b", Result: "r", CostUsd: 0.1, DurationMs: 5000, Turns: 3,
	}}}}
	res, err := qt.read(context.Background(), toolReq(map[string]any{"id": "t1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); !strings.Contains(got, "outcome: $0.1000 · 5s · 3 turn(s)") {
		t.Errorf("queue_read missing the outcome:\n%s", got)
	}
}
