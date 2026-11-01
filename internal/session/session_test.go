package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestLastTurn(t *testing.T) {
	tests := []struct {
		name        string
		msgs        []Message
		wantRequest string
		wantReply   string
	}{
		{name: "empty", msgs: nil},
		{name: "latest of each", msgs: []Message{
			{Role: "user", Text: "first ask"}, {Role: "assistant", Text: "first answer"},
			{Role: "user", Text: "second   ask\nwith lines"}, {Role: "assistant", Text: "second answer"},
		}, wantRequest: "second ask with lines", wantReply: "second answer"},
		{name: "harness bookkeeping is not a request", msgs: []Message{
			{Role: "user", Text: "real ask"}, {Role: "assistant", Text: "answer"},
			{Role: "user", Text: "<command-name>/exit</command-name>"},
		}, wantRequest: "real ask", wantReply: "answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, rep := LastTurn(tt.msgs)
			if req != tt.wantRequest || rep != tt.wantReply {
				t.Errorf("LastTurn = %q, %q", req, rep)
			}
		})
	}
	long := strings.Repeat("é", maxTurnChars)
	_, rep := LastTurn([]Message{{Role: "assistant", Text: long}})
	if !utf8.ValidString(rep) || !strings.HasSuffix(rep, "…") || len(rep) > maxTurnChars+len("…") {
		t.Errorf("long reply not clipped cleanly: %d bytes", len(rep))
	}
}

func TestReadCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCheckpoint(dir, &Record{SessionID: "s", LastRequest: "ask", LastReply: "reply"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCheckpoint(dir, "s")
	if err != nil || got.LastRequest != "ask" || got.LastReply != "reply" {
		t.Errorf("ReadCheckpoint = %+v, %v", got, err)
	}
	if _, err := ReadCheckpoint(dir, "missing"); err == nil {
		t.Errorf("missing checkpoint read without error")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.checkpoint.yaml"), []byte(":\n\t- ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCheckpoint(dir, "bad"); err == nil {
		t.Errorf("corrupt checkpoint read without error")
	}
}

// Harness context logged next to the turns must not read as something the user
// said: it is most of a transcript's text and repeats in every session.
func TestParseTranscriptKeepsOnlySpokenText(t *testing.T) {
	lines := []string{
		`{"type":"api-request-blob","message":{"role":"user","content":[{"type":"text","text":"Contents of CLAUDE.md: always use tabs"}]}}`,
		`{"type":"system","message":{"role":"system","content":"hook success"}}`,
		`{"type":"attachment","message":{"role":"user","content":"attached context"}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"Caveat: local commands below"}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued"}}`,
		`{"type":"user","message":{"role":"user","content":"<system-reminder>\ngitStatus: clean\n</system-reminder>"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>skills</system-reminder>\nfix the flaky socket test"}]}}`,
		`{"type":"user","message":{"role":"user","content":"<task-notification>done</task-notification>"}}`,
		`{"type":"user","message":{"role":"user","content":"[SYSTEM NOTIFICATION - NOT USER INPUT]\\nA task ended."}}`,
		`{"type":"user","message":{"role":"user","content":"<agent-message from=\\"a1\\">[Subagent hand-back] report</agent-message>"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fixed it"}]}}`,
		`{"message":{"role":"user","content":"a line with no type is still a turn"}}`,
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Role+": "+m.Text)
	}
	want := []string{"user: fix the flaky socket test", "assistant: fixed it", "user: a line with no type is still a turn"}
	if !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

func TestStripHarnessTextLeavesOrdinaryAngleBrackets(t *testing.T) {
	const s = "compare a<b and <T> generics; <system-reminder> never closed"
	if got := stripHarnessText(s); got != s {
		t.Fatalf("stripHarnessText(%q) = %q", s, got)
	}
}

func TestStripHarnessTextPairsTags(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a<system-reminder>x</system-reminder>b", "ab"},
		{"a<agent-message from=\"z\">x</agent-message>b", "ab"},
		{"keep <system-reminder>x</agent-message> this", "keep <system-reminder>x</agent-message> this"},
		{"<system-reminder-x>kept</system-reminder-x>", "<system-reminder-x>kept</system-reminder-x>"},
		{"one<task-notification>x</task-notification>two<task-notification>y</task-notification>three", "onetwothree"},
		{"[SYSTEM NOTIFICATION about the kiln", "[SYSTEM NOTIFICATION about the kiln"},
	}
	for _, tt := range tests {
		if got := stripHarnessText(tt.in); got != tt.want {
			t.Errorf("stripHarnessText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAssistantTurnsKeepTagsTheyDiscuss(t *testing.T) {
	path := writeTranscript(t, `{"type":"assistant","message":{"role":"assistant","content":"the <system-reminder>x</system-reminder> block"}}`+"\n")
	msgs, err := ParseTranscript(path)
	if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0].Text, "<system-reminder>") {
		t.Fatalf("%+v, %v", msgs, err)
	}
}
