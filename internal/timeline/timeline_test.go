package timeline

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMarkerAndSpan(t *testing.T) {
	a, b := at("2026-10-21T14:02:59Z"), at("2026-10-22T09:10:00-07:00")
	text := Marker(b) + " user: later\n\n" + Marker(a) + " assistant: earlier\n\n[2026-13-45T99:99Z] junk"
	first, last, ok := Span(text)
	if !ok || !first.Equal(at("2026-10-21T14:02:00Z")) || !last.Equal(at("2026-10-22T16:10:00Z")) {
		t.Fatalf("span = %v %v %v", first, last, ok)
	}
	if Marker(a) != "[2026-10-21T14:02Z]" {
		t.Fatalf("marker = %s", Marker(a))
	}
	if _, _, ok := Span("no dates here, nor [2026-10-21 14:02 UTC]"); ok {
		t.Fatal("found a span in undated text")
	}
}

func TestRange(t *testing.T) {
	a := at("2026-10-21T14:02:00Z")
	for _, tt := range []struct {
		start, end time.Time
		want       string
	}{
		{time.Time{}, time.Time{}, "unknown"},
		{a, time.Time{}, "2026-10-21 14:02 UTC"},
		{time.Time{}, a, "2026-10-21 14:02 UTC"},
		{a, a, "2026-10-21 14:02 UTC"},
		{a, at("2026-10-21T15:40:00Z"), "2026-10-21 14:02 to 15:40 UTC"},
		{a, at("2026-10-22T09:10:00Z"), "2026-10-21 14:02 to 2026-10-22 09:10 UTC"},
	} {
		if got := Range(tt.start, tt.end); got != tt.want {
			t.Errorf("Range(%v, %v) = %q, want %q", tt.start, tt.end, got, tt.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	a := at("2026-10-21T14:02:00Z").Unix()
	for _, tt := range []struct {
		start, end int64
		typ, want  string
	}{
		{a, a + 3600, "session_transcript", "2026-10-21 14:02 to 15:02 UTC"},
		{0, 0, "session_record", "date unknown"},
		{0, 0, "repo_doc", ""},
		{a, 0, "manual", "2026-10-21 14:02 UTC"},
	} {
		if got := Describe(tt.start, tt.end, tt.typ); got != tt.want {
			t.Errorf("Describe(%d, %d, %s) = %q, want %q", tt.start, tt.end, tt.typ, got, tt.want)
		}
	}
}

func TestNeutralize(t *testing.T) {
	quoted := "see [1970-01-01T00:00Z] and [9999-12-31T23:59Z] here, not [note]"
	out := Neutralize(quoted)
	if _, _, ok := Span(out); ok {
		t.Fatalf("neutralized text still dates: %q", out)
	}
	if out != "see (1970-01-01T00:00Z) and (9999-12-31T23:59Z) here, not [note]" {
		t.Fatalf("Neutralize = %q", out)
	}
}
