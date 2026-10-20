package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/handoff"
)

func toolInput(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestPostToolUseHandsOffEditedDocs(t *testing.T) {
	tests := []struct {
		name     string
		suffix   string
		wantFile bool
	}{
		{name: "markdown is handed off", suffix: "notes.md", wantFile: true},
		{name: "non-md non-config ignored", suffix: "main.go", wantFile: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			path := filepath.Join(t.TempDir(), tt.suffix)
			if err := os.WriteFile(path, []byte("# doc"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			p := Payload{ToolName: "Write", ToolInput: toolInput(t, map[string]string{"file_path": path})}
			if err := postToolUse(p); err != nil {
				t.Fatalf("postToolUse: %v", err)
			}
			files, _ := handoff.Files(handoff.Dir(), time.Now().Add(time.Minute))
			if got := len(files) == 1 && files[0].Path == path; got != tt.wantFile {
				t.Errorf("handed off %v, want %v", files, tt.wantFile)
			}
		})
	}
}

func TestPostToolUseRequestsPRSweep(t *testing.T) {
	tests := []struct {
		name    string
		command string
		isGHPR  bool
	}{
		{name: "gh pr create asks for a refresh", command: "gh pr create --fill", isGHPR: true},
		{name: "gh pr view does not", command: "gh pr view 5", isGHPR: false},
		{name: "unrelated bash ignored", command: "ls -la", isGHPR: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			p := Payload{ToolName: "Bash", ToolInput: toolInput(t, map[string]string{"command": tt.command})}
			if err := postToolUse(p); err != nil {
				t.Errorf("postToolUse must not error: %v", err)
			}
			if got := handoff.TakePRSweep(handoff.Dir()); got != tt.isGHPR {
				t.Errorf("refresh requested = %v, want %v", got, tt.isGHPR)
			}
		})
	}
}
