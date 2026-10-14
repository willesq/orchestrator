package agentrun

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestRunPassesRoutedModel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantModel bool
		wantFail  string
	}{
		{name: "routed model", model: "opus", wantModel: true},
		{name: "pinned id", model: "claude-fable-5-1[1m]", wantModel: true},
		{name: "unrouted", model: ""},
		{name: "flag-shaped model fails before running", model: "--dangerously-skip-permissions", wantFail: "invalid model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.reply(t, `{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
			got, err := f.runner.Run(context.Background(), Task{ID: "task-1", Agent: "dev", Body: "b", Cwd: t.TempDir(), Model: tt.model})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFail != "" {
				if got.Status != StatusFailed || !strings.Contains(got.Result, tt.wantFail) {
					t.Fatalf("outcome = %+v", got)
				}
				return
			}
			args := strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n")
			i := slices.Index(args, "--model")
			if !tt.wantModel {
				if i >= 0 {
					t.Fatalf("unrouted task passed --model: %q", args)
				}
				return
			}
			if i < 0 || i+1 >= len(args) || args[i+1] != tt.model {
				t.Fatalf("args = %q, want --model %s", args, tt.model)
			}
		})
	}
}
