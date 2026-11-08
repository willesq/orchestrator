package config

import (
	"slices"
	"strings"
	"testing"
)

func TestQueueWorkerAllowedTools(t *testing.T) {
	t.Run("claude gets a read-only default", func(t *testing.T) {
		v := loadTemp(t)
		var err error
		if err != nil {
			t.Fatal(err)
		}
		s, err := Unmarshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Queue.Worker.AllowedTools["claude"]
		if !slices.Contains(got, "Bash(git status:*)") || !slices.Contains(got, "Bash(go test:*)") {
			t.Errorf("claude defaults = %v", got)
		}
		for _, r := range got {
			if strings.Contains(r, "dispatch") || strings.Contains(r, "push") || strings.Contains(r, "rm") {
				t.Errorf("default grants a mutating rule: %q", r)
			}
		}
	})
	t.Run("lists are set per harness from the CLI", func(t *testing.T) {
		loadTemp(t)
		if err := applyTemp(t, map[string]any{"queue.worker.allowed_tools": map[string]any{
			"claude": []string{"Bash(make test:*)", "Bash(ls:*)"}, "codex": []string{"shell(ls)"},
		}}, true); err != nil {
			t.Fatal(err)
		}
		s, err := Unmarshal(reload(t))
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Queue.Worker.AllowedTools["claude"]; !slices.Equal(got, []string{"Bash(make test:*)", "Bash(ls:*)"}) {
			t.Errorf("claude = %v", got)
		}
		if got := s.Queue.Worker.AllowedTools["codex"]; !slices.Equal(got, []string{"shell(ls)"}) {
			t.Errorf("codex = %v", got)
		}
	})
}

func TestQueueVerifyDefaults(t *testing.T) {
	v := loadTemp(t)
	var err error
	if err != nil {
		t.Fatal(err)
	}
	s, err := Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Queue.Worker.Verify; !got.Enabled || got.Timeout != "15m" || got.Retries != 1 {
		t.Errorf("verify defaults = %+v", got)
	}
}
