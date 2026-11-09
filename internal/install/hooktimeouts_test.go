package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// orchCommand returns the orch-hook command object registered for event, or nil.
func orchCommand(t *testing.T, root map[string]any, event string) map[string]any {
	t.Helper()
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	for _, item := range arr {
		if !isOrchHook(item, testBins().OrchHook) {
			continue
		}
		inner := item.(map[string]any)["hooks"].([]any)
		return inner[0].(map[string]any)
	}
	return nil
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}

func TestClaudeHookTimeouts(t *testing.T) {
	tests := []struct {
		event     string
		wantSecs  float64
		wantAsync bool
	}{
		{event: "SessionStart", wantSecs: 10},
		{event: "UserPromptSubmit", wantSecs: 10},
		{event: "Stop", wantSecs: 5},
		{event: "SessionEnd", wantSecs: 15},
		{event: "PreCompact", wantSecs: 15},
		{event: "PreToolUse", wantSecs: 5},
		{event: "PostToolUse", wantSecs: 5, wantAsync: true},
		{event: "SubagentStop", wantSecs: 5},
		{event: "CwdChanged", wantSecs: 5, wantAsync: true},
	}
	if len(tests) != len(eventSlugs) {
		t.Fatalf("table covers %d events, installer wires %d", len(tests), len(eventSlugs))
	}
	home := t.TempDir()
	if err := BuildPlan(ClaudeCode{}, testBins(), home).Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	root := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			cmd := orchCommand(t, root, tt.event)
			if cmd == nil {
				t.Fatalf("no orch-hook entry for %s", tt.event)
			}
			// JSON numbers decode as float64.
			got, ok := cmd["timeout"].(float64)
			if !ok || got != tt.wantSecs {
				t.Errorf("timeout = %v, want %v seconds", cmd["timeout"], tt.wantSecs)
			}
			// Claude Code caps per-hook timeouts that raise the SessionEnd budget at 60 s.
			if got <= 0 || got > 60 {
				t.Errorf("timeout %v outside (0, 60]", got)
			}
			async, has := cmd["async"]
			if tt.wantAsync && async != true {
				t.Errorf("async = %v, want true", async)
			}
			if !tt.wantAsync && has {
				t.Errorf("async set on %s, want absent", tt.event)
			}
		})
	}
}

func TestGeminiHookTimeouts(t *testing.T) {
	tests := []struct {
		event  string
		wantMS float64
	}{
		{event: "SessionStart", wantMS: 10000},
		{event: "BeforeAgent", wantMS: 10000},
		{event: "SessionEnd", wantMS: 15000},
		{event: "AfterTool", wantMS: 5000},
	}
	if len(tests) != len(geminiEvents) {
		t.Fatalf("table covers %d events, installer wires %d", len(tests), len(geminiEvents))
	}
	home := t.TempDir()
	if err := BuildPlan(Gemini{}, testBins(), home).Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	root := readJSON(t, filepath.Join(home, ".gemini", "settings.json"))
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			cmd := orchCommand(t, root, tt.event)
			if cmd == nil {
				t.Fatalf("no orch-hook entry for %s", tt.event)
			}
			if got, _ := cmd["timeout"].(float64); got != tt.wantMS {
				t.Errorf("timeout = %v, want %v ms", cmd["timeout"], tt.wantMS)
			}
			// Gemini's hook configuration has no async field.
			if _, has := cmd["async"]; has {
				t.Errorf("async written for Gemini, which does not support it")
			}
		})
	}
}

// TestCodexHooksUnchanged pins Codex's hook entries to the bare command shape:
// timeouts were verified for Claude Code and Gemini only.
func TestCodexHooksUnchanged(t *testing.T) {
	home := t.TempDir()
	if err := BuildPlan(Codex{}, testBins(), home).Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	root := readJSON(t, filepath.Join(home, ".codex", "hooks.json"))
	for _, ev := range codexEvents {
		cmd := orchCommand(t, root, ev.Event)
		if cmd == nil {
			t.Fatalf("no orch-hook entry for %s", ev.Event)
		}
		if len(cmd) != 2 {
			t.Errorf("%s command has keys %v, want only type and command", ev.Event, cmd)
		}
	}
}

// TestHookTimeoutUpgrade covers an existing install from before timeouts: the
// bare orch-hook entries must read as drift, apply must add the fields in place
// with a backup, keep third-party hooks (including their own timeouts), and a
// repeat apply must be a no-op.
func TestHookTimeoutUpgrade(t *testing.T) {
	bin := testBins().OrchHook
	orca := func() map[string]any {
		return map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "/orca/claude-hook.sh", "timeout": 10,
		}}}
	}
	tests := []struct {
		name     string
		adapter  Adapter
		rel      string
		events   eventMap
		wantKeys func(slug string) map[string]any
	}{
		{name: "claude-code", adapter: ClaudeCode{}, rel: ".claude/settings.json", events: eventSlugs, wantKeys: claudeHookFields},
		{name: "gemini", adapter: Gemini{}, rel: ".gemini/settings.json", events: geminiEvents, wantKeys: geminiHookFields},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, tt.rel)
			hooks := map[string]any{}
			for _, ev := range tt.events {
				// Third-party first, then ours as an older install wrote it.
				hooks[ev.Event] = []any{orca(), orchEntry(bin, ev.Slug)}
			}
			old, _ := json.MarshalIndent(map[string]any{"hooks": hooks, "theme": "dark"}, "", "  ")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, old, 0o600); err != nil {
				t.Fatal(err)
			}

			plan := BuildPlan(tt.adapter, testBins(), home)
			var dry bytes.Buffer
			if err := plan.Print(&dry); err != nil {
				t.Fatalf("Print: %v", err)
			}
			if !strings.Contains(dry.String(), "MODIFY "+path) || !strings.Contains(dry.String(), "timeout=") {
				t.Errorf("dry-run does not show the timeout diff:\n%s", dry.String())
			}
			if b, _ := os.ReadFile(path); !bytes.Equal(b, old) {
				t.Fatalf("dry-run modified %s", path)
			}

			backups := filepath.Join(t.TempDir(), "b1")
			if err := plan.Apply(&bytes.Buffer{}, backups); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(snapshot(t, backups)) == 0 {
				t.Errorf("apply over an existing file wrote no backup")
			}
			root := readJSON(t, path)
			if root["theme"] != "dark" {
				t.Errorf("unrelated setting lost: %v", root["theme"])
			}
			for _, ev := range tt.events {
				arr := root["hooks"].(map[string]any)[ev.Event].([]any)
				if len(arr) != 2 {
					t.Fatalf("%s: %d entries, want 2", ev.Event, len(arr))
				}
				third := arr[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
				if third["command"] != "/orca/claude-hook.sh" || third["timeout"] != float64(10) {
					t.Errorf("%s: third-party entry changed: %v", ev.Event, third)
				}
				if !isOrchHook(arr[1], bin) {
					t.Fatalf("%s: orch-hook entry moved from its slot", ev.Event)
				}
				cmd := arr[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
				for k, want := range tt.wantKeys(ev.Slug) {
					got := cmd[k]
					if n, ok := want.(int); ok {
						want = float64(n)
					}
					if got != want {
						t.Errorf("%s: %s = %v, want %v", ev.Event, k, got, want)
					}
				}
			}

			n, err := plan.Pending()
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			if n != 0 {
				t.Errorf("Pending() = %d after apply, want 0", n)
			}
			backups2 := filepath.Join(t.TempDir(), "b2")
			if err := plan.Apply(&bytes.Buffer{}, backups2); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			if len(snapshot(t, backups2)) != 0 {
				t.Errorf("repeat apply wrote backups; want a no-op")
			}
		})
	}
}
