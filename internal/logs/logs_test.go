package logs

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func messages(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Source+"|"+e.Message)
	}
	return out
}

func TestRingKeepsTheNewest(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 5; i++ {
		r.Add("orchd", fmt.Sprintf("line %d", i))
	}
	got, last, oldest := r.Since(0, "", 0)
	if !slices.Equal(messages(got), []string{"orchd|line 3", "orchd|line 4", "orchd|line 5"}) || last != 5 || oldest != 3 {
		t.Errorf("Since = %v last %d oldest %d", messages(got), last, oldest)
	}
	got, _, _ = r.Since(4, "", 0)
	if !slices.Equal(messages(got), []string{"orchd|line 5"}) {
		t.Errorf("after 4 = %v", messages(got))
	}
	if got, last, _ := r.Since(5, "", 0); len(got) != 0 || last != 5 {
		t.Errorf("nothing new = %v, %d", got, last)
	}
}

func TestSinceFiltersAndLimitsToTheNewest(t *testing.T) {
	r := NewRing(10)
	r.Add("qdrant", "a")
	r.Add("orchd", "b")
	r.Add("qdrant", "c")
	r.Add("qdrant", "d")
	got, _, _ := r.Since(0, "qdrant", 2)
	if !slices.Equal(messages(got), []string{"qdrant|c", "qdrant|d"}) {
		t.Errorf("filtered = %v", messages(got))
	}
	if !slices.Equal(r.Sources(), []string{"orchd", "qdrant"}) {
		t.Errorf("sources = %v", r.Sources())
	}
	empty := NewRing(0)
	if _, _, oldest := empty.Since(0, "", 0); oldest != 0 {
		t.Error("an empty ring has an oldest entry")
	}
}

func TestAddCleansAndDropsEmpty(t *testing.T) {
	r := NewRing(5)
	r.now = func() time.Time { return time.Unix(10, 0) }
	r.Add("qdrant", "\x1b[32mINFO\x1b[0m  served‮\t")
	r.Add("qdrant", "   ")
	r.Add("orchd", strings.Repeat("é", maxMessage))
	got, _, _ := r.Since(0, "", 0)
	if len(got) != 2 || got[0].Message != "INFO  served" || !got[0].Time.Equal(time.Unix(10, 0)) {
		t.Errorf("entries = %+v", got)
	}
	if !strings.HasSuffix(got[1].Message, "…") || len(got[1].Message) > maxMessage+4 {
		t.Errorf("long line not capped: %d bytes", len(got[1].Message))
	}
}

func TestAttribute(t *testing.T) {
	tests := []struct{ line, source, message string }{
		{"2026/10/23 18:19:24 docs: synced 3 repos", "docs-scheduler", "synced 3 repos"},
		{"2026/10/23 18:19:24 pr-babysitter: rate limited", "pr-babysitter", "rate limited"},
		{"2026/10/23 18:19:24 goose: migrated", "database", "migrated"},
		{"2026/10/23 18:19:24 INFO Applying migrations...", "database", "INFO Applying migrations..."},
		{"2026/10/23 18:19:24 OK   0007_models.sql (250.29µs)", "database", "OK   0007_models.sql (250.29µs)"},
		{"2026/10/23 18:19:24 OK, done", "orchd", "OK, done"},
		{"2026/10/23 18:19:24 orchd 0.19.0 listening on x", "orchd", "orchd 0.19.0 listening on x"},
		{"2026/10/23 18:19:24 orchd: model routing is off", "orchd", "model routing is off"},
		{"2026/10/23 18:19:24 session abc: summary skipped", "orchd", "session abc: summary skipped"},
		{"2026/10/23 18:19:24 embed: loading model", "rag", "loading model"},
		{"no timestamp here", "orchd", "no timestamp here"},
	}
	for _, tt := range tests {
		s, m := Attribute(tt.line)
		if s != tt.source || m != tt.message {
			t.Errorf("Attribute(%q) = %q, %q; want %q, %q", tt.line, s, m, tt.source, tt.message)
		}
	}
}

func TestWritersSplitLinesAndKeepPartials(t *testing.T) {
	r := NewRing(10)
	w := r.LogWriter()
	_, _ = w.Write([]byte("2026/10/23 18:19:24 docs: one\n2026/10/23 18:19:24 hand"))
	_, _ = w.Write([]byte("off: two\n"))
	q := r.SourceWriter("qdrant")
	_, _ = q.Write([]byte("partial"))
	got, _, _ := r.Since(0, "", 0)
	if !slices.Equal(messages(got), []string{"docs-scheduler|one", "handoff|two"}) {
		t.Errorf("entries = %v", messages(got))
	}
	_, _ = q.Write([]byte(" line\n"))
	if got, _, _ := r.Since(2, "", 0); !slices.Equal(messages(got), []string{"qdrant|partial line"}) {
		t.Errorf("child line = %v", messages(got))
	}
	// A line with no newline that grows past the cap is flushed rather than held.
	_, _ = q.Write([]byte(strings.Repeat("x", 4*maxMessage+1)))
	if got, _, _ := r.Since(3, "qdrant", 0); len(got) != 1 {
		t.Errorf("oversized partial not flushed: %d entries", len(got))
	}
}

func TestConcurrentWrites(t *testing.T) {
	r := NewRing(1000)
	w := r.SourceWriter("qdrant")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = w.Write([]byte("line\n"))
				r.Add("orchd", "x")
				_, _, _ = r.Since(0, "", 10)
			}
		}()
	}
	wg.Wait()
	if _, last, _ := r.Since(0, "", 0); last != 800 {
		t.Errorf("last = %d, want 800", last)
	}
}

func TestLineWriterReleasesAGrownBuffer(t *testing.T) {
	r := NewRing(10)
	w := r.SourceWriter("qdrant").(*lineWriter)
	_, _ = w.Write([]byte(strings.Repeat("y", 20*maxMessage) + "\n"))
	if w.pend != nil {
		t.Errorf("buffer of cap %d kept after an oversized line", cap(w.pend))
	}
	_, _ = w.Write([]byte("short\n"))
	if got, _, _ := r.Since(0, "", 0); len(got) != 2 || got[1].Message != "short" {
		t.Errorf("entries = %v", got)
	}
}
