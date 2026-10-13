package taskfmt

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestOutcome(t *testing.T) {
	tests := []struct {
		name string
		task *orchdv1.Task
		want []string
	}{
		{name: "unfinished task renders nothing", task: &orchdv1.Task{Status: "pending"}, want: nil},
		{name: "finished task", task: &orchdv1.Task{
			CostUsd: 0.4213, DurationMs: 61234, Turns: 17, SessionId: "s1",
			Models: []string{"claude-haiku-4-5", "claude-sonnet-5-5"}, Prs: []string{"https://github.com/acme/w/pull/1"},
		}, want: []string{"outcome: $0.4213 · 1m1.2s · 17 turn(s)", "models:  claude-haiku-4-5, claude-sonnet-5-5", "session: s1", "prs:     https://github.com/acme/w/pull/1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Outcome(tt.task)
			if tt.want == nil && got != "" {
				t.Fatalf("got %q, want nothing", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

func TestStats(t *testing.T) {
	tasks := []*orchdv1.Task{
		{Agent: "dev", Status: "complete", CostUsd: 1, DurationMs: 60000},
		{Agent: "dev", Status: "complete", CostUsd: 2, DurationMs: 120000},
		{Agent: "dev", Status: "failed", CostUsd: 0.5, DurationMs: 30000},
		{Agent: "dev", Status: "in_progress"},
		{Agent: "research", Status: "blocked", CostUsd: 0.25, DurationMs: 10000},
	}
	stats := Stats(tasks)
	if len(stats) != 2 || stats[0].Agent != "dev" {
		t.Fatalf("want dev first: %+v", stats)
	}
	dev := stats[0]
	if dev.Total != 4 || dev.Complete != 2 || dev.Failed != 1 || dev.Open != 1 || dev.CostUSD != 3.5 {
		t.Errorf("dev = %+v", dev)
	}
	if got := dev.SuccessRate(); got < 0.66 || got > 0.67 {
		t.Errorf("success rate = %v, want 2/3", got)
	}
	if dev.AvgDurationMS() != 70000 {
		t.Errorf("avg = %d, want 70000 (open tasks excluded)", dev.AvgDurationMS())
	}
	if (AgentStats{}).SuccessRate() != 0 || (AgentStats{}).AvgDurationMS() != 0 {
		t.Errorf("empty stats must be zero")
	}
	table := StatsTable(stats)
	for _, w := range []string{"AGENT", "dev", "research", "67%", "$3.50", "all", "$3.75"} {
		if !strings.Contains(table, w) {
			t.Errorf("table missing %q:\n%s", w, table)
		}
	}
}
