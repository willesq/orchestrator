package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTranscriptTurnsAreDated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	body := `{"type":"user","timestamp":"2026-10-20T09:00:41.123Z","message":{"role":"user","content":"start"}}` + "\n" +
		`{"type":"assistant","timestamp":"not a time","message":{"role":"assistant","content":[{"type":"text","text":"undated"}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-10-21T16:45:00Z","message":{"role":"user","content":"what did we do yesterday"}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseTranscript(p)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if !msgs[1].Time.IsZero() {
		t.Fatal("a malformed time was kept")
	}
	text := TranscriptText(msgs)
	for _, want := range []string{"[2026-10-20T09:00Z] user: start", "\n\nassistant: undated", "[2026-10-21T16:45Z] user: what did we do yesterday"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript text lacks %q:\n%s", want, text)
		}
	}
	start, end := Span(msgs)
	if start.Format(time.RFC3339) != "2026-10-20T09:00:41Z" || end.Format(time.RFC3339) != "2026-10-21T16:45:00Z" {
		t.Fatalf("span = %v %v", start, end)
	}
	rec := BuildRecord("s", "/x", p, msgs)
	if rec.StartedAt != "2026-10-20T09:00:41Z" || rec.EndedAt != "2026-10-21T16:45:00Z" {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.HasPrefix(rec.Text(), "Session 2026-10-20 09:00 to 2026-10-21 16:45 UTC\n\n") {
		t.Fatalf("record text = %q", rec.Text())
	}
}

func TestUndatedRecord(t *testing.T) {
	rec := BuildRecord("s", "", "", []Message{{Role: "user", Text: "hi"}})
	if rec.StartedAt != "" || rec.EndedAt != "" || rec.Text() != rec.Summary {
		t.Fatalf("record = %+v, text %q", rec, rec.Text())
	}
	if s, e := rec.Span(); !s.IsZero() || !e.IsZero() {
		t.Fatal("an undated record has a span")
	}
	if s, e := Span(nil); !s.IsZero() || !e.IsZero() {
		t.Fatal("no messages have a span")
	}
}

func TestQuotedMarkersDoNotDateTranscripts(t *testing.T) {
	when := time.Date(2026, 10, 21, 14, 2, 0, 0, time.UTC)
	text := TranscriptText([]Message{{Role: "user", Time: when, Text: "the page said [1970-01-01T00:00Z] and [9999-12-31T23:59Z]"}})
	if strings.Count(text, "Z]") != 1 || !strings.HasPrefix(text, "[2026-10-21T14:02Z] user:") || !strings.Contains(text, "(1970-01-01T00:00Z)") {
		t.Fatalf("text = %q", text)
	}
}
