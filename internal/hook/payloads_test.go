package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordedPayloadsParse decodes every payload recorded from a real
// harness (scripts/hook-capture.sh, spaces plan SP-001) into Payload and
// checks that cwd, tool_name and tool_input survive unchanged.
func TestRecordedPayloadsParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "payloads", "*.jsonl"))
	if err != nil || len(files) < 2 {
		t.Fatalf("want fixtures for Claude Code and Codex, got %v (%v)", files, err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 1<<20)
			parsed, tools := 0, 0
			for line := 1; sc.Scan(); line++ {
				var rec struct {
					Event   string          `json:"event"`
					Payload json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
					t.Fatalf("line %d: %v", line, err)
				}
				var raw map[string]json.RawMessage
				var p Payload
				if err := json.Unmarshal(rec.Payload, &raw); err != nil {
					t.Fatalf("line %d: payload is not an object: %v", line, err)
				}
				if err := json.Unmarshal(rec.Payload, &p); err != nil {
					t.Fatalf("line %d (%s): decode into Payload: %v", line, rec.Event, err)
				}
				var cwd, tool string
				_ = json.Unmarshal(raw["cwd"], &cwd)
				_ = json.Unmarshal(raw["tool_name"], &tool)
				if cwd == "" || p.Cwd != cwd {
					t.Errorf("line %d (%s): cwd %q, Payload.Cwd %q", line, rec.Event, cwd, p.Cwd)
				}
				if p.ToolName != tool {
					t.Errorf("line %d (%s): tool_name %q, Payload.ToolName %q", line, rec.Event, tool, p.ToolName)
				}
				if in, ok := raw["tool_input"]; ok {
					tools++
					if !jsonEqual(t, in, p.ToolInput) {
						t.Errorf("line %d (%s): tool_input changed:\n%s\n%s", line, rec.Event, in, p.ToolInput)
					}
				}
				parsed++
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			if parsed == 0 || tools == 0 {
				t.Errorf("parsed %d payloads, %d with tool_input; want both > 0", parsed, tools)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
