package taskfmt

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestRoute(t *testing.T) {
	tests := []struct {
		r    *orchdv1.Route
		want string
	}{
		{&orchdv1.Route{TaskKind: "code", Provider: "claude-code", Model: "opus", Confidence: "low", Rationale: "declared frontier"},
			"code → claude-code/opus (confidence low, 0 outcome(s); declared frontier)"},
		{&orchdv1.Route{Provider: "codex", Model: "gpt-sol", Effort: "high"}, "codex/gpt-sol@high"},
		{&orchdv1.Route{TaskKind: "code", Rationale: "no routable model"}, "code → agent default (no routable model)"},
	}
	for _, tt := range tests {
		if got := Route(tt.r); got != tt.want {
			t.Errorf("Route = %q, want %q", got, tt.want)
		}
	}
}

func TestOutcomeShowsRouteBeforeFinish(t *testing.T) {
	got := Outcome(&orchdv1.Task{Status: "pending", Route: &orchdv1.Route{TaskKind: "docs", Provider: "claude-code", Model: "sonnet"}})
	if got != "route:   docs → claude-code/sonnet\n" {
		t.Fatalf("got %q", got)
	}
	done := Outcome(&orchdv1.Task{Status: "complete", Turns: 2, DurationMs: 1000, Route: &orchdv1.Route{Model: "opus", Provider: "claude-code"}})
	if !strings.HasPrefix(done, "route:   claude-code/opus\noutcome:") {
		t.Fatalf("got %q", done)
	}
}
