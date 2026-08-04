package session

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTranscript(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestParseTranscript(t *testing.T) {
	tests := []struct {
		name  string
		lines string
		want  int
	}{
		{
			name: "string and block content",
			lines: `{"type":"user","message":{"role":"user","content":"hello there"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}
`,
			want: 2,
		},
		{
			name: "malformed and empty lines skipped",
			lines: `not json
{"type":"user","message":{"role":"user","content":""}}
{"type":"assistant","message":{"role":"assistant","content":"real"}}
`,
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTranscript(t, tt.lines)
			msgs, err := ParseTranscript(path)
			if err != nil {
				t.Fatalf("ParseTranscript: %v", err)
			}
			if len(msgs) != tt.want {
				t.Errorf("parsed %d messages, want %d", len(msgs), tt.want)
			}
		})
	}
}

func TestParseTranscriptMissingFile(t *testing.T) {
	tests := []struct{ name string }{{name: "missing file errors"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseTranscript("/nonexistent/x.jsonl"); err == nil {
				t.Errorf("expected error for missing transcript")
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []Message
		maxChars int
		wantSub  string
	}{
		{name: "prefers assistant text", msgs: []Message{{Role: "user", Text: "q"}, {Role: "assistant", Text: "the answer is X"}}, maxChars: 280, wantSub: "answer is X"},
		{name: "falls back to first message", msgs: []Message{{Role: "user", Text: "only user"}}, maxChars: 280, wantSub: "only user"},
		{name: "truncates", msgs: []Message{{Role: "assistant", Text: "abcdefghij"}}, maxChars: 3, wantSub: "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Summarize(tt.msgs, tt.maxChars)
			if !contains(got, tt.wantSub) {
				t.Errorf("Summarize = %q, want substring %q", got, tt.wantSub)
			}
		})
	}
}

func TestBuildAndWriteRecord(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint bool
	}{
		{name: "authoritative record", checkpoint: false},
		{name: "checkpoint record", checkpoint: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rec := BuildRecord("sid", "/cwd", "/t.jsonl", []Message{{Role: "assistant", Text: "hi"}})
			var (
				err  error
				file string
			)
			if tt.checkpoint {
				err = WriteCheckpoint(dir, rec)
				file = filepath.Join(dir, "sid.checkpoint.yaml")
			} else {
				err = WriteRecord(dir, rec)
				file = filepath.Join(dir, "sid.yaml")
			}
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Errorf("record file not written: %v", err)
			}
		})
	}
}

func TestDeleteCheckpoint(t *testing.T) {
	tests := []struct {
		name      string
		precreate bool
	}{
		{name: "removes existing", precreate: true},
		{name: "missing is not an error", precreate: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.precreate {
				_ = WriteCheckpoint(dir, BuildRecord("sid", "", "", nil))
			}
			if err := DeleteCheckpoint(dir, "sid"); err != nil {
				t.Errorf("DeleteCheckpoint: %v", err)
			}
		})
	}
}

func TestTranscriptText(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []Message
		wantSub string
	}{
		{name: "renders role prefixes", msgs: []Message{{Role: "user", Text: "q"}, {Role: "assistant", Text: "a"}}, wantSub: "assistant: a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TranscriptText(tt.msgs); !contains(got, tt.wantSub) {
				t.Errorf("TranscriptText = %q, want substring %q", got, tt.wantSub)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
