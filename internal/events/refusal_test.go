package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var refusalAt = time.Date(2026, 11, 14, 9, 30, 0, 0, time.FixedZone("x", 3600))

// TestNewRefusal: the event carries the checked fields and a summary built
// from them alone; a field of the wrong shape is replaced or dropped.
func TestNewRefusal(t *testing.T) {
	cases := []struct {
		name        string
		in          Refusal
		want        Refusal
		wantSummary string
	}{
		{"guard refusal",
			Refusal{Session: "sess-1", Space: "acme", Tool: "Bash", Rule: "other_job_path"},
			Refusal{Session: "sess-1", Space: "acme", Tool: "Bash", Rule: "other_job_path"},
			"refused a Bash call (rule other_job_path) in space acme, session sess-1"},
		{"term",
			Refusal{Session: "s", Space: "acme", Tool: "Bash", Rule: "outbound_term", Term: &TermRef{Space: "globex", Line: 3, Hash: "0123456789ab"}},
			Refusal{Session: "s", Space: "acme", Tool: "Bash", Rule: "outbound_term", Term: &TermRef{Space: "globex", Line: 3, Hash: "0123456789ab"}},
			"refused a Bash call (rule outbound_term) in space acme, session s; a term of space globex, terms file line 3 (hash 0123456789ab)"},
		{"no space or session",
			Refusal{Tool: "mcp__srv__write_file", Rule: "pin_missing"},
			Refusal{Tool: "mcp__srv__write_file", Rule: "pin_missing"},
			"refused a mcp__srv__write_file call (rule pin_missing) in no space"},
		{"hostile fields",
			Refusal{Session: "../etc/passwd", Space: "Acme Corp\n", Tool: "Bash; rm -rf /", Rule: "/Users/x/secret",
				Term: &TermRef{Space: "globex", Line: 1, Hash: "zephyr"}},
			Refusal{Tool: "unknown", Rule: "unknown"},
			"refused a unknown call (rule unknown) in no space"},
		{"term without a line",
			Refusal{Tool: "Bash", Rule: "outbound_term", Term: &TermRef{Space: "globex", Hash: "ab"}},
			Refusal{Tool: "Bash", Rule: "outbound_term"},
			"refused a Bash call (rule outbound_term) in no space"},
		{"term of a malformed space",
			Refusal{Tool: "Bash", Rule: "outbound_term", Term: &TermRef{Space: "Globex Inc", Line: 2, Hash: "ab"}},
			Refusal{Tool: "Bash", Rule: "outbound_term"},
			"refused a Bash call (rule outbound_term) in no space"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewRefusal(tc.in, refusalAt)
			if e.Kind != KindRefusal || e.Source != "guard" || e.Timestamp != "2026-11-14T08:30:00Z" || e.Refusal == nil {
				t.Fatalf("event %+v", e)
			}
			got, _ := json.Marshal(e.Refusal)
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Errorf("refusal %s, want %s", got, want)
			}
			if e.Summary != tc.wantSummary {
				t.Errorf("summary %q, want %q", e.Summary, tc.wantSummary)
			}
		})
	}
	// The term is copied, so changing the caller's TermRef later does not
	// change the event.
	ref := &TermRef{Space: "globex", Line: 3, Hash: "ab"}
	e := NewRefusal(Refusal{Tool: "Bash", Rule: "outbound_term", Term: ref}, refusalAt)
	ref.Line = 9
	if e.Refusal.Term.Line != 3 {
		t.Errorf("term line %d after the caller's change", e.Refusal.Term.Line)
	}
}

// TestAppendSpaces: refusals go to the spaces stream, which is made
// owner-only, also when it already existed with a wider mode; they read
// back with their fields, beside the stream's other events.
func TestAppendSpaces(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, SpacesStream, Event{Kind: "info", Source: "enforcement", Summary: "guard off"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SpacesStream+".jsonl")
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v before", fi.Mode().Perm())
	}
	r := Refusal{Session: "s", Space: "acme", Tool: "Bash", Rule: "outbound_term", Term: &TermRef{Space: "globex", Line: 1, Hash: "ab"}}
	if err := AppendSpaces(dir, NewRefusal(r, refusalAt)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	evs, err := Read(dir, SpacesStream)
	if err != nil || len(evs) != 2 {
		t.Fatalf("read %v, %v", evs, err)
	}
	if evs[0].Refusal != nil || evs[1].Refusal == nil || *evs[1].Refusal.Term != *r.Term || evs[1].Refusal.Rule != r.Rule {
		t.Errorf("events %+v", evs)
	}
	raw, _ := os.ReadFile(path)
	if first := strings.SplitN(string(raw), "\n", 2)[0]; strings.Contains(first, "refusal") {
		t.Errorf("an event without a refusal carries the field: %s", first)
	}
	// A new stream is created owner-only.
	dir2 := filepath.Join(t.TempDir(), "events")
	if err := AppendSpaces(dir2, NewRefusal(r, refusalAt)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir2, SpacesStream+".jsonl")); fi.Mode().Perm() != 0o600 {
		t.Errorf("new stream mode %v", fi.Mode().Perm())
	}
}

// TestAppendSpacesErrors: an events path that is a file, or a stream
// that cannot be opened, is reported.
func TestAppendSpacesErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewRefusal(Refusal{Tool: "Bash", Rule: "pin_missing"}, refusalAt)
	if err := AppendSpaces(file, e); err == nil {
		t.Error("events dir is a file: no error")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, SpacesStream+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AppendSpaces(dir, e); err == nil {
		t.Error("stream is a directory: no error")
	}
}

// TestAppendSpacesRefusesOddFiles: a FIFO or a symlink planted at the
// stream's path is refused at once, never waited on or followed.
func TestAppendSpacesRefusesOddFiles(t *testing.T) {
	e := NewRefusal(Refusal{Tool: "Bash", Rule: "pin_missing"}, refusalAt)
	dir := t.TempDir()
	path := filepath.Join(dir, SpacesStream+".jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { done <- AppendSpaces(dir, e) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("FIFO: no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO: the write blocked")
	}
	// With a reader waiting the open succeeds; the FIFO is still refused.
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := AppendSpaces(dir, e); err != ErrNotRegular {
		t.Errorf("FIFO with a reader: %v, want ErrNotRegular", err)
	}

	dir2 := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir2, SpacesStream+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := AppendSpaces(dir2, e); err == nil {
		t.Error("symlink: no error")
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("written through the link: %s", b)
	}
}
