package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func toolInput(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestPostToolUseReingest(t *testing.T) {
	tests := []struct {
		name       string
		suffix     string
		underHome  bool
		wantIngest bool
		wantType   string
	}{
		{name: "markdown re-ingested as repo_doc", suffix: "notes.md", wantIngest: true, wantType: "repo_doc"},
		{name: "non-md non-config ignored", suffix: "main.go", wantIngest: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.suffix)
			if err := os.WriteFile(path, []byte("# doc with a ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 token"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			fc := &fakeClient{}
			p := Payload{ToolName: "Write", ToolInput: toolInput(t, map[string]string{"file_path": path})}
			if err := postToolUse(p, factory(fc, nil)); err != nil {
				t.Fatalf("postToolUse: %v", err)
			}
			if (fc.ingestCalls > 0) != tt.wantIngest {
				t.Errorf("ingest calls = %d, wantIngest %v", fc.ingestCalls, tt.wantIngest)
			}
		})
	}
}

func TestPostToolUseBash(t *testing.T) {
	tests := []struct {
		name    string
		command string
		isGHPR  bool
	}{
		{name: "gh pr create matches", command: "gh pr create --fill", isGHPR: true},
		{name: "unrelated bash ignored", command: "ls -la", isGHPR: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Only asserting it never errors (the sweep shells out to gh; may fail).
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			p := Payload{ToolName: "Bash", ToolInput: toolInput(t, map[string]string{"command": tt.command})}
			if err := postToolUse(p, factory(&fakeClient{}, nil)); err != nil {
				t.Errorf("postToolUse must not error: %v", err)
			}
			if ghPRRe.MatchString(tt.command) != tt.isGHPR {
				t.Errorf("gh pr match = %v, want %v", ghPRRe.MatchString(tt.command), tt.isGHPR)
			}
		})
	}
}

func TestClassifyPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		name     string
		path     string
		wantOK   bool
		wantType string
	}{
		{name: "native config", path: home + "/.claude/CLAUDE.md", wantOK: true, wantType: "claude_config"},
		{name: "markdown", path: "/tmp/x/readme.md", wantOK: true, wantType: "repo_doc"},
		{name: "other", path: "/tmp/x/main.go", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _, ok := classifyPath(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && st != tt.wantType {
				t.Errorf("type = %q, want %q", st, tt.wantType)
			}
		})
	}
}
