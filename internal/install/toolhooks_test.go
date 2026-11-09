package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestToolHookInstall is SP-012.2.a1: upgrading an earlier orch install adds
// the tool hooks for Claude Code (PreToolUse, CwdChanged) and Codex
// (PreToolUse) — dry-run by default, backed up, idempotent on repeat, and
// keeping a third-party hook on the same events.
func TestToolHookInstall(t *testing.T) {
	bin := testBins().OrchHook
	thirdParty := func() map[string]any {
		return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/rtk/rtk-rewrite.sh", "timeout": 3}}}
	}
	tests := []struct {
		name    string
		adapter Adapter
		rel     string
		older   eventMap // the events an install from before SP-012 wired
		want    []string
		absent  []string
	}{
		{
			name: "claude-code", adapter: ClaudeCode{}, rel: ".claude/settings.json",
			older: slices.DeleteFunc(slices.Clone(eventSlugs), func(e struct{ Slug, Event string }) bool {
				return e.Slug == "pre-tool-use" || e.Slug == "cwd-changed"
			}),
			want: []string{"PreToolUse", "CwdChanged"},
		},
		{
			name: "codex", adapter: Codex{}, rel: ".codex/hooks.json",
			older: slices.DeleteFunc(slices.Clone(codexEvents), func(e struct{ Slug, Event string }) bool {
				return e.Slug == "pre-tool-use"
			}),
			want: []string{"PreToolUse"}, absent: []string{"CwdChanged"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, tt.rel)
			hooks := map[string]any{}
			for _, ev := range tt.older {
				hooks[ev.Event] = []any{orchEntry(bin, ev.Slug)}
			}
			for _, ev := range tt.want {
				hooks[ev] = []any{thirdParty()}
			}
			old, _ := json.MarshalIndent(map[string]any{"hooks": hooks, "theme": "dark"}, "", "  ")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, old, 0o644); err != nil {
				t.Fatal(err)
			}
			plan := BuildPlan(tt.adapter, testBins(), home)

			// Dry run by default: Print writes nothing and names the change.
			if cov, _ := ToolHooks(tt.name, testBins(), home); !reflect.DeepEqual(cov.Missing, tt.want) {
				t.Fatalf("before install, missing = %v, want %v", cov.Missing, tt.want)
			}
			var dry bytes.Buffer
			if err := plan.Print(&dry); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(dry.String(), "MODIFY "+path) {
				t.Errorf("dry run does not report the change to %s:\n%s", path, dry.String())
			}
			if b, _ := os.ReadFile(path); !bytes.Equal(b, old) {
				t.Fatal("dry run wrote the hooks file")
			}

			// Apply backs the original up and wires the new events.
			backups := filepath.Join(t.TempDir(), "b1")
			if err := plan.Apply(&bytes.Buffer{}, backups); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(mapsValues(snapshot(t, backups)), string(old)) {
				t.Errorf("no backup of the original %s", tt.rel)
			}
			cov, err := ToolHooks(tt.name, testBins(), home)
			if err != nil || !cov.Covered || !reflect.DeepEqual(cov.Wired, tt.want) || len(cov.Missing) != 0 {
				t.Fatalf("after install, coverage = %+v, %v", cov, err)
			}
			root := readJSON(t, path)
			got := root["hooks"].(map[string]any)
			for _, ev := range tt.want {
				// The third-party hook keeps its place and its fields.
				entries := got[ev].([]any)
				if len(entries) != 2 || !reflect.DeepEqual(roundTrip(t, entries[0]), roundTrip(t, thirdParty())) {
					t.Errorf("%s entries = %v; want the third-party hook first, then orch-hook", ev, entries)
				}
			}
			for _, ev := range tt.absent {
				if _, ok := got[ev]; ok {
					t.Errorf("%s wired for %s, which has no such event", ev, tt.name)
				}
			}
			if root["theme"] != "dark" {
				t.Error("unrelated setting lost")
			}

			// A repeat apply writes and backs up nothing.
			before := snapshot(t, home)
			again := filepath.Join(t.TempDir(), "b2")
			if err := plan.Apply(&bytes.Buffer{}, again); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, snapshot(t, home)) {
				t.Error("second apply changed files")
			}
			if n := len(snapshot(t, again)); n != 0 {
				t.Errorf("second apply wrote %d backups", n)
			}
		})
	}
}

// TestClaudeToolHookFields: pre-tool-use is synchronous, so the tool guard
// (SP-041) can deny through it; cwd-changed decides nothing and runs in the
// background.
func TestClaudeToolHookFields(t *testing.T) {
	if f := claudeHookFields("pre-tool-use"); f["async"] != nil || f["timeout"] != 5 {
		t.Errorf("pre-tool-use fields = %v", f)
	}
	if f := claudeHookFields("cwd-changed"); f["async"] != true || f["timeout"] != 5 {
		t.Errorf("cwd-changed fields = %v", f)
	}
}

// TestToolHooksCoverage: a harness spaces do not cover reports so, and an
// unreadable hooks file is an error rather than "nothing wired".
func TestToolHooksCoverage(t *testing.T) {
	home := t.TempDir()
	if cov, err := ToolHooks("gemini", testBins(), home); err != nil || cov.Covered {
		t.Errorf("gemini = %+v, %v; want not covered", cov, err)
	}
	cov, err := ToolHooks("codex", testBins(), home)
	if err != nil || !cov.Covered || !reflect.DeepEqual(cov.Missing, []string{"PreToolUse"}) {
		t.Errorf("codex without hooks.json = %+v, %v", cov, err)
	}
	path := filepath.Join(home, ".codex", "hooks.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := ToolHooks("codex", testBins(), home); err == nil {
		t.Error("a malformed hooks.json read as no hooks")
	}
	// A command that only mentions orch-hook is not orch's hook.
	_ = os.WriteFile(path, []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x; /opt/orch/orch-hook pre-tool-use"}]}]}}`), 0o644)
	if cov, _ := ToolHooks("codex", testBins(), home); len(cov.Wired) != 0 {
		t.Errorf("look-alike command counted as wired: %+v", cov)
	}
	_ = os.Chmod(path, 0)
	defer os.Chmod(path, 0o644)
	if os.Geteuid() != 0 {
		if _, err := ToolHooks("codex", testBins(), home); err == nil {
			t.Error("an unreadable hooks.json read as no hooks")
		}
	}
}

func mapsValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
