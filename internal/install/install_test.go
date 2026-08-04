package install

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBins() Bins {
	return Bins{Orch: "/opt/orch/orch", OrchHook: "/opt/orch/orch-hook", Orchd: "/opt/orch/orchd"}
}

func TestAdapterFor(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "claude-code", target: "claude-code", wantErr: false},
		{name: "unknown", target: "emacs", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := AdapterFor(tt.target)
			if (err != nil) != tt.wantErr {
				t.Errorf("AdapterFor(%q) err = %v, wantErr %v", tt.target, err, tt.wantErr)
			}
		})
	}
}

func TestApplyHooksSurgical(t *testing.T) {
	tests := []struct {
		name          string
		existing      map[string]any
		wantPreserved bool // a non-orchestrator entry survives
	}{
		{
			name:     "empty settings gets 7 events",
			existing: map[string]any{},
		},
		{
			name: "preserves third-party, replaces orchestrator",
			existing: map[string]any{
				"hooks": map[string]any{
					"UserPromptSubmit": []any{
						map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/orch/orch-hook user-prompt-submit"}}},
						map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/some/orca/claude-hook.sh"}}},
					},
				},
			},
			wantPreserved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			maps.Copy(root, tt.existing)
			summary := applyHooks(root, "/opt/orch/orch-hook")
			hooks := root["hooks"].(map[string]any)
			if len(hooks) != len(eventSlugs) {
				t.Errorf("got %d events, want %d", len(hooks), len(eventSlugs))
			}
			ups := hooks["UserPromptSubmit"].([]any)
			// exactly one orchestrator entry after apply
			orchCount, orcaFound := 0, false
			for _, item := range ups {
				cmd := item.(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
				if strings.Contains(cmd, "orch-hook") {
					orchCount++
				}
				if strings.Contains(cmd, "orca") {
					orcaFound = true
				}
			}
			if orchCount != 1 {
				t.Errorf("orchestrator entries = %d, want 1 (summary: %s)", orchCount, summary)
			}
			if tt.wantPreserved && !orcaFound {
				t.Errorf("third-party Orca hook was not preserved")
			}
		})
	}
}

func TestApplyMCP(t *testing.T) {
	tests := []struct {
		name          string
		existing      map[string]any
		wantPreserved int
	}{
		{name: "empty", existing: map[string]any{}, wantPreserved: 0},
		{
			name: "preserves serena",
			existing: map[string]any{
				"mcpServers": map[string]any{"serena": map[string]any{"command": "serena"}},
			},
			wantPreserved: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			maps.Copy(root, tt.existing)
			applyMCP(root, "/opt/orch/orch")
			mcp := root["mcpServers"].(map[string]any)
			if _, ok := mcp["rag"]; !ok {
				t.Errorf("rag not registered")
			}
			if _, ok := mcp["queue"]; !ok {
				t.Errorf("queue not registered")
			}
			if tt.wantPreserved == 1 {
				if _, ok := mcp["serena"]; !ok {
					t.Errorf("serena not preserved")
				}
			}
		})
	}
}

func TestPlanPrintAndApply(t *testing.T) {
	tests := []struct {
		name  string
		apply bool
	}{
		{name: "dry-run writes nothing", apply: false},
		{name: "apply writes files", apply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			var out bytes.Buffer
			settings := filepath.Join(home, ".claude", "settings.json")

			if tt.apply {
				if err := plan.Apply(&out, filepath.Join(t.TempDir(), "backups")); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				b, err := os.ReadFile(settings)
				if err != nil {
					t.Fatalf("settings not written: %v", err)
				}
				var root map[string]any
				if err := json.Unmarshal(b, &root); err != nil {
					t.Fatalf("settings not valid JSON: %v", err)
				}
				if _, ok := root["hooks"]; !ok {
					t.Errorf("hooks not written")
				}
			} else {
				if err := plan.Print(&out); err != nil {
					t.Fatalf("Print: %v", err)
				}
				if _, err := os.Stat(settings); !os.IsNotExist(err) {
					t.Errorf("dry-run must not write settings.json")
				}
				if !strings.Contains(out.String(), "DRY RUN") {
					t.Errorf("dry-run banner missing")
				}
			}
		})
	}
}

func TestApplyBacksUpExisting(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "existing file is backed up before overwrite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(settings, []byte(`{"mcpServers":{"serena":{"command":"serena"}}}`), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			backupDir := filepath.Join(t.TempDir(), "backups")
			var out bytes.Buffer
			if err := BuildPlan(ClaudeCode{}, testBins(), home).Apply(&out, backupDir); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			// The prior settings.json must have been backed up.
			found := false
			_ = filepath.WalkDir(backupDir, func(_ string, d fs.DirEntry, _ error) error {
				if d != nil && !d.IsDir() {
					found = true
				}
				return nil
			})
			if !found {
				t.Errorf("no backup written for the overwritten settings.json")
			}
			// serena must survive the surgical rewrite.
			b, _ := os.ReadFile(settings)
			if !strings.Contains(string(b), "serena") {
				t.Errorf("serena MCP not preserved after apply")
			}
		})
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func TestApplyIsIdempotent(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "second apply changes nothing and backs up nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)

			var out1 bytes.Buffer
			if err := plan.Apply(&out1, filepath.Join(t.TempDir(), "b1")); err != nil {
				t.Fatalf("first apply: %v", err)
			}
			before := snapshot(t, home)

			backup2 := filepath.Join(t.TempDir(), "b2")
			var out2 bytes.Buffer
			if err := plan.Apply(&out2, backup2); err != nil {
				t.Fatalf("second apply: %v", err)
			}
			after := snapshot(t, home)

			if len(before) != len(after) {
				t.Fatalf("file count changed: %d -> %d", len(before), len(after))
			}
			for p, c := range before {
				if after[p] != c {
					t.Errorf("second apply changed %s", p)
				}
			}
			if !strings.Contains(out2.String(), "already fully installed") {
				t.Errorf("second apply should report no-op, got:\n%s", out2.String())
			}
			// No backups should have been written on the idempotent second run.
			backups := snapshot(t, backup2)
			if len(backups) != 0 {
				t.Errorf("second apply wrote %d backup file(s), want 0", len(backups))
			}
		})
	}
}

func TestPrintUnchangedAfterInstall(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "dry-run after install reports already installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			if err := plan.Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var out bytes.Buffer
			if err := plan.Print(&out); err != nil {
				t.Fatalf("print: %v", err)
			}
			if !strings.Contains(out.String(), "Already fully installed") {
				t.Errorf("expected already-installed message, got:\n%s", out.String())
			}
		})
	}
}

func TestAliasEdit(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		wantVerb string
	}{
		{name: "adds block to empty", existing: "", wantVerb: "added"},
		{name: "adds after existing content", existing: "export FOO=1\n", wantVerb: "added"},
		{name: "updates existing block", existing: aliasBegin + "\nalias orch=\"/old\"\n" + aliasEnd + "\n", wantVerb: "updated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := aliasEdit("/opt/orch/orch", t.TempDir())
			nw, summary, err := e.Compute([]byte(tt.existing))
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			s := string(nw)
			if strings.Count(s, aliasBegin) != 1 {
				t.Errorf("expected exactly one managed block, got:\n%s", s)
			}
			if !strings.Contains(s, `alias orch="/opt/orch/orch"`) {
				t.Errorf("alias line missing:\n%s", s)
			}
			if tt.existing != "" && !strings.HasPrefix(tt.existing, aliasBegin) && !strings.Contains(s, "export FOO=1") && strings.Contains(tt.existing, "FOO") {
				t.Errorf("pre-existing content lost")
			}
			if !strings.Contains(summary, tt.wantVerb) {
				t.Errorf("summary %q missing verb %q", summary, tt.wantVerb)
			}
		})
	}
}

func TestPlistEdit(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "contains orchd path and KeepAlive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := plistEdit("/opt/orch/orchd", t.TempDir())
			nw, summary, err := e.Compute(nil)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			s := string(nw)
			if !strings.Contains(s, "/opt/orch/orchd") || !strings.Contains(s, "KeepAlive") {
				t.Errorf("plist missing content:\n%s", s)
			}
			if !strings.Contains(summary, "launchd") {
				t.Errorf("summary missing: %s", summary)
			}
		})
	}
}
