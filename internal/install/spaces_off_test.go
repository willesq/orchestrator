package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookEntryFor is one hooks-file entry running command.
func hookEntryFor(command string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}}
}

// spacesInstallFixture is a hooks file an install made while spaces were on:
// orch's tool hooks, a third party's PreToolUse hook, and orch's Stop hook.
func spacesInstallFixture(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{hookEntryFor("/o/orch-hook pre-tool-use"), hookEntryFor("/usr/local/bin/safety-check")},
		"CwdChanged": []any{hookEntryFor("/o/orch-hook cwd-changed")},
		"Stop":       []any{hookEntryFor("/o/orch-hook stop")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSpacesOffInstallIsPreSpaces is the install half of "disabled =
// pre-spaces behaviour": with spaces off, Claude Code and Codex get no tool
// hooks, orch's own tool hook entries are removed, every third-party entry
// and every other orch hook is kept, and a second run changes nothing.
func TestSpacesOffInstallIsPreSpaces(t *testing.T) {
	defer SetSpacesLoader(func() (bool, error) { return false, nil })()
	bins := Bins{Orch: "/o/orch", OrchHook: "/o/orch-hook", Orchd: "/o/orchd"}
	edits := map[string]Edit{
		"claude-code": ClaudeCode{}.settingsEdit(bins, t.TempDir()),
		"codex":       codexHooksEdit(bins, t.TempDir()),
	}
	for name, edit := range edits {
		t.Run(name+"/fresh", func(t *testing.T) {
			out, _, err := edit.Compute(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, slug := range []string{"orch-hook pre-tool-use", "orch-hook cwd-changed"} {
				if bytes.Contains(out, []byte(slug)) {
					t.Errorf("%s written with spaces off", slug)
				}
			}
			for _, slug := range []string{"orch-hook session-start", "orch-hook post-tool-use", "orch-hook stop"} {
				if !bytes.Contains(out, []byte(slug)) {
					t.Errorf("%s missing: a pre-spaces hook must still be written", slug)
				}
			}
		})
		t.Run(name+"/after a spaces install", func(t *testing.T) {
			out, summary, err := edit.Compute(spacesInstallFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			var root struct {
				Hooks map[string][]any `json:"hooks"`
			}
			if err := json.Unmarshal(out, &root); err != nil {
				t.Fatal(err)
			}
			pre := root.Hooks["PreToolUse"]
			if len(pre) != 1 || !strings.Contains(mustJSON(t, pre), "/usr/local/bin/safety-check") {
				t.Fatalf("PreToolUse = %s, want only the third-party entry", mustJSON(t, pre))
			}
			// Codex has no CwdChanged; orch never wrote one there.
			if _, ok := root.Hooks["CwdChanged"]; ok && name == "claude-code" {
				t.Fatalf("CwdChanged kept: %s", mustJSON(t, root.Hooks["CwdChanged"]))
			}
			if !strings.Contains(summary, "removed orch's PreToolUse") {
				t.Errorf("summary does not say what was removed:\n%s", summary)
			}
			again, _, err := edit.Compute(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(normalized(t, again), normalized(t, out)) {
				t.Fatalf("a second run changed the file:\n%s\nthen\n%s", out, again)
			}
		})
	}
}

// TestSpacesSwitchUnreadableKeepsToolHooks: a switch that cannot be read
// counts as on, so install never drops hooks a policy may depend on.
func TestSpacesSwitchUnreadableKeepsToolHooks(t *testing.T) {
	defer SetSpacesLoader(func() (bool, error) { return false, errors.New("database is locked") })()
	if SpacesOff() {
		t.Fatal("an unreadable switch reads as off")
	}
	bins := Bins{Orch: "/o/orch", OrchHook: "/o/orch-hook", Orchd: "/o/orchd"}
	out, _, err := ClaudeCode{}.settingsEdit(bins, t.TempDir()).Compute(spacesInstallFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("orch-hook pre-tool-use")) || !bytes.Contains(out, []byte("orch-hook cwd-changed")) {
		t.Fatalf("tool hooks dropped:\n%s", out)
	}
}

// TestSpacesOffInstallAppliesToATempHome runs the Claude Code plan for a
// temporary home with spaces off: the file it writes has no tool hooks, its
// previous content is backed up, and the plan is then idempotent.
func TestSpacesOffInstallAppliesToATempHome(t *testing.T) {
	defer SetSpacesLoader(func() (bool, error) { return false, nil })()
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, spacesInstallFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	bins := Bins{Orch: "/o/orch", OrchHook: "/o/orch-hook", Orchd: "/o/orchd"}
	plan := Plan{Target: "claude-code", Edits: []Edit{ClaudeCode{}.settingsEdit(bins, home)}}
	backups := filepath.Join(home, "backups")
	if err := plan.Apply(&bytes.Buffer{}, backups); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("orch-hook pre-tool-use")) || bytes.Contains(got, []byte("orch-hook cwd-changed")) {
		t.Fatalf("tool hooks still installed:\n%s", got)
	}
	if entries, err := os.ReadDir(backups); err != nil || len(entries) == 0 {
		t.Fatalf("no backup of the changed file: %v", err)
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Fatalf("pending after apply = %d, %v", n, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func normalized(t *testing.T, b []byte) []byte {
	t.Helper()
	out, err := normalizeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
