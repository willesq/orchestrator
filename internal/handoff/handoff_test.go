package handoff

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var later = time.Now().Add(time.Minute)

func TestSessionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := PutSession(dir, Session{ID: "s1", TranscriptPath: "/t/a.jsonl", Tags: []string{"checkout:/x"}, Record: true}); err != nil {
		t.Fatal(err)
	}
	// A second end of the same session replaces the entry.
	if err := PutSession(dir, Session{ID: "s1", TranscriptPath: "/t/b.jsonl"}); err != nil {
		t.Fatal(err)
	}
	got, err := Sessions(dir, later)
	if err != nil || len(got) != 1 || got[0].TranscriptPath != "/t/b.jsonl" || got[0].Record {
		t.Fatalf("sessions = %+v, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "sessions", "s1.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if got, _ := Sessions(dir, time.Now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("an entry newer than the cutoff was listed: %+v", got)
	}
	if err := DoneSession(dir, got[0]); err != nil {
		t.Fatal(err)
	}
	if err := DoneSession(dir, got[0]); err != nil {
		t.Fatalf("removing a missing entry: %v", err)
	}
	if got, _ := Sessions(dir, later); len(got) != 0 {
		t.Fatalf("after done = %+v", got)
	}
}

func TestSessionIDsAreChecked(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../x", "a/b"} {
		if PutSession(dir, Session{ID: id}) == nil || DoneSession(dir, Session{ID: id}) == nil || FailSession(dir, Session{ID: id}) == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

func TestTamperedEntriesAreDropped(t *testing.T) {
	dir := t.TempDir()
	if err := PutSession(dir, Session{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	sdir := filepath.Join(dir, "sessions")
	// A file named for one session but holding another, and one that is not JSON.
	_ = os.WriteFile(filepath.Join(sdir, "other.json"), []byte(`{"session_id":"good"}`), 0o600)
	_ = os.WriteFile(filepath.Join(sdir, "junk.json"), []byte(`not json`), 0o600)
	_ = os.WriteFile(filepath.Join(sdir, "half.123.tmp"), []byte(`{`), 0o600)
	old := filepath.Join(sdir, "stale.456.tmp")
	_ = os.WriteFile(old, []byte(`{`), 0o600)
	_ = os.Chtimes(old, time.Now().Add(-2*staleTemp), time.Now().Add(-2*staleTemp))
	got, _ := Sessions(dir, later)
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("sessions = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(sdir, "half.123.tmp")); err != nil {
		t.Error("a fresh temporary file was removed")
	}
	for _, name := range []string{"other.json", "junk.json", "stale.456.tmp"} {
		if _, err := os.Stat(filepath.Join(sdir, name)); !os.IsNotExist(err) {
			t.Errorf("%s kept", name)
		}
	}
}

func TestFilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for range 3 { // a burst of edits is one entry
		if err := PutFile(dir, File{Path: "/r/README.md"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := PutFile(dir, File{Path: "/r/other.md"}); err != nil {
		t.Fatal(err)
	}
	got, err := Files(dir, later)
	if err != nil || len(got) != 2 {
		t.Fatalf("files = %+v, %v", got, err)
	}
	for _, f := range got {
		if f.Path == "/r/README.md" {
			if err := DoneFile(dir, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, _ := Files(dir, later); len(got) != 1 || got[0].Path != "/r/other.md" {
		t.Fatalf("after done = %+v", got)
	}
	if got, err := Files(t.TempDir(), later); err != nil || got != nil {
		t.Fatalf("empty dir = %+v, %v", got, err)
	}
}

func TestPRSweepRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if TakePRSweep(dir) {
		t.Fatal("no request was made")
	}
	if err := RequestPRSweep(dir); err != nil {
		t.Fatal(err)
	}
	if err := RequestPRSweep(dir); err != nil {
		t.Fatal(err)
	}
	if !TakePRSweep(dir) || TakePRSweep(dir) {
		t.Fatal("a request is taken exactly once")
	}
}

func TestClassify(t *testing.T) {
	home := "/Users/me"
	tests := []struct {
		path, wantType, wantURI string
		ok                      bool
	}{
		{"/Users/me/.claude/CLAUDE.md", "claude_config", "claude://CLAUDE.md", true},
		{"/Users/me/.claude/settings.json", "claude_config", "claude://settings.json", true},
		{"/tmp/x/README.MD", "repo_doc", "file:///tmp/x/README.MD", true},
		{"/tmp/x/main.go", "", "", false},
		{"notes.md", "", "", false},                        // relative paths are refused
		{"/Users/me/.claude/../.ssh/id.md", "", "", false}, // unclean paths too
	}
	for _, tt := range tests {
		st, uri, ok := Classify(tt.path, home)
		if ok != tt.ok || st != tt.wantType || uri != tt.wantURI {
			t.Errorf("Classify(%q) = %q %q %v", tt.path, st, uri, ok)
		}
	}
	if _, _, ok := Classify("/x/.claude/a.json", ""); ok {
		t.Error("no home: a non-markdown file was accepted")
	}
}

func TestDir(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", "/data")
	if Dir() != "/data/handoff" {
		t.Fatalf("Dir = %s", Dir())
	}
}

func TestRewrittenEntriesSurvive(t *testing.T) {
	dir := t.TempDir()
	if err := PutSession(dir, Session{ID: "s1", TranscriptPath: "/t/a.jsonl"}); err != nil {
		t.Fatal(err)
	}
	read, _ := Sessions(dir, later)
	// The session ends again while orchd works on the first entry.
	time.Sleep(10 * time.Millisecond)
	if err := PutSession(dir, Session{ID: "s1", TranscriptPath: "/t/b.jsonl"}); err != nil {
		t.Fatal(err)
	}
	if err := RetrySession(dir, read[0]); err != nil {
		t.Fatal(err)
	}
	if err := DoneSession(dir, read[0]); err != nil {
		t.Fatal(err)
	}
	now, _ := Sessions(dir, later)
	if len(now) != 1 || now[0].TranscriptPath != "/t/b.jsonl" || now[0].Attempts != 0 {
		t.Fatalf("newer entry = %+v", now)
	}
	// An unchanged entry is retried and then failed aside.
	if err := RetrySession(dir, now[0]); err != nil {
		t.Fatal(err)
	}
	now, _ = Sessions(dir, later)
	if len(now) != 1 || now[0].Attempts != 1 {
		t.Fatalf("after retry = %+v", now)
	}
	if err := FailSession(dir, now[0]); err != nil {
		t.Fatal(err)
	}
	if left, _ := Sessions(dir, later); len(left) != 0 {
		t.Fatalf("failed entry still pending: %+v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "sessions", "s1.json")); err != nil {
		t.Fatalf("failed entry not kept: %v", err)
	}
	if err := FailSession(dir, now[0]); err != nil {
		t.Fatalf("failing a missing entry: %v", err)
	}
}

func TestFileRetryAndFail(t *testing.T) {
	dir := t.TempDir()
	if err := PutFile(dir, File{Path: "/r/a.md"}); err != nil {
		t.Fatal(err)
	}
	got, _ := Files(dir, later)
	if err := RetryFile(dir, got[0]); err != nil {
		t.Fatal(err)
	}
	got, _ = Files(dir, later)
	if got[0].Attempts != 1 {
		t.Fatalf("attempts = %d", got[0].Attempts)
	}
	if err := FailFile(dir, got[0]); err != nil {
		t.Fatal(err)
	}
	if left, _ := Files(dir, later); len(left) != 0 {
		t.Fatalf("left = %+v", left)
	}
	// Retrying an entry that is gone does nothing.
	if err := RetryFile(dir, got[0]); err != nil {
		t.Fatal(err)
	}
	if left, _ := Files(dir, later); len(left) != 0 {
		t.Fatalf("retry recreated an entry: %+v", left)
	}
}

func TestPRSweepRequestRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(dir, "pr-sweep")); err != nil {
		t.Fatal(err)
	}
	if RequestPRSweep(dir) == nil {
		t.Fatal("followed a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("created the symlink's target")
	}
}
