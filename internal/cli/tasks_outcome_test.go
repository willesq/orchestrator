package cli

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestTaskOutcomeAndStats(t *testing.T) {
	finished := &orchdv1.Task{Id: "t1", Agent: "dev", Status: "complete", Body: "fix it", Result: "done",
		CostUsd: 0.5, DurationMs: 90000, Turns: 12, Models: []string{"claude-opus-5-5"}, Prs: []string{"https://github.com/acme/w/pull/3"}}
	list := &orchdv1.ListTasksResponse{Tasks: []*orchdv1.Task{
		finished,
		{Id: "t2", Agent: "dev", Status: "failed", CostUsd: 0.25, DurationMs: 30000},
		{Id: "t3", Agent: "research", Status: "pending"},
	}}
	tests := []struct {
		name   string
		args   []string
		client *fakeClient
		want   []string
	}{
		{name: "task view shows the outcome", args: []string{"task", "t1"},
			client: &fakeClient{getTask: &orchdv1.GetTaskResponse{Found: true, Task: finished}},
			want:   []string{"outcome: $0.5000 · 1m30s · 12 turn(s)", "models:  claude-opus-5-5", "prs:     https://github.com/acme/w/pull/3"}},
		{name: "stats summarize per agent", args: []string{"tasks", "--stats"},
			client: &fakeClient{listTasks: list},
			want:   []string{"AGENT", "dev", "research", "50%", "$0.75", "all"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
		})
	}
}
