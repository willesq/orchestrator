package toollog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toollog.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func newWriter(t *testing.T, s *Store, queue int) *Writer {
	t.Helper()
	w := NewWriter(s, queue, nil)
	t.Cleanup(w.Close)
	return w
}

func flush(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func bashEvent(cmd string) func() Record {
	return func() Record {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		return Record{Event: "pre-tool-use", Tool: "Bash", Command: Command("Bash", in), Cwd: "/w"}
	}
}

func session(id string) Owner { return Owner{Kind: OwnerSession, ID: id} }

// key charges o's events to o itself.
func key(o Owner) Key { return Key{Quota: o.String(), Owner: o} }

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestEventsStoredInOrderOwnerOnly is SP-014.1.a1 at the store: events of
// each session keep the order they were handed in, interleaved sessions do
// not mix, and the database, -wal and -shm files are mode 0600.
func TestEventsStoredInOrderOwnerOnly(t *testing.T) {
	s, path := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	for i := range 50 {
		for _, id := range []string{"a", "b"} {
			if !w.Enqueue(key(session(id)), 100, bashEvent(fmt.Sprintf("echo %s-%d", id, i))) {
				t.Fatal("Enqueue dropped an event with an empty queue")
			}
		}
	}
	flush(t, w)
	for _, id := range []string{"a", "b"} {
		recs, err := s.Records(context.Background(), session(id))
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 50 {
			t.Fatalf("session %s: %d records, want 50", id, len(recs))
		}
		for i, r := range recs {
			if want := fmt.Sprintf("echo %s-%d", id, i); r.Command != want || r.Seq != int64(i+1) {
				t.Errorf("session %s record %d = seq %d %q, want seq %d %q", id, i, r.Seq, r.Command, i+1, want)
			}
		}
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), m)
		}
	}
}

func TestOpenTightensAndRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Run("wider files are tightened", func(t *testing.T) {
		path := filepath.Join(dir, "wide.db")
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if m := modeOf(t, p); m != 0o600 {
				t.Errorf("%s mode = %o, want 600", filepath.Base(p), m)
			}
		}
	})
	t.Run("symlink refused", func(t *testing.T) {
		target := filepath.Join(dir, "elsewhere.db")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.db")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Open(symlink) = %v, want not a regular file", err)
		}
	})
	t.Run("directory refused", func(t *testing.T) {
		if _, err := Open(t.TempDir()); err == nil {
			t.Error("Open(directory) succeeded")
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		if _, err := Open(filepath.Join(dir, "no", "such", "toollog.db")); err == nil {
			t.Error("Open under a missing directory succeeded")
		}
	})
	t.Run("newer schema refused", func(t *testing.T) {
		s, path := openTemp(t)
		if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
			t.Errorf("Open = %v, want a newer-schema error", err)
		}
	})
}

// credentialFixtures are SP-014.1.a2's fixtures: one per kind named in the
// acceptance item, written the way they appear in commands.
var credentialFixtures = []struct {
	name, cmd, secret string
}{
	{"gh classic", "gh api -H 'Authorization: token ghp_R4nd0mT0kenValue1234567890abcdEFGH' /user", "ghp_R4nd0mT0ken"},
	{"gh fine-grained", "GH_TOKEN=github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz0123456789 gh pr list", "github_pat_11ABC"},
	{"gh in header", `curl -H "Authorization: Bearer gho_16C7e42F292c6912E7710c838347Ae178B4a" https://api.github.com`, "gho_16C7e42F"},
	{"aws key id", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE aws s3 ls", "AKIAIOSFODNN7EXAMPLE"},
	{"aws secret", "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY && aws sts get-caller-identity", "wJalrXUtnFEMI"},
	{"aws session token", "aws configure set aws_session_token FwoGZXIvYXdzEJr//////////wEaDExampleToken0123 && aws_session_token=FwoGZXIvYXdzEJr//////////wEaDExampleToken0123 aws s3 ls", "aws_session_token=FwoGZXIv"},
	{"anthropic", "ANTHROPIC_API_KEY=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 claude -p hi", "sk-ant-api03-AbCd"},
	{"jwt", "curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U' https://x", "eyJhbGciOiJIUzI1NiJ9"},
	{"jwt bare", "echo eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U | jq", "dozjgNryP4J3"},
	{"aws secret after a space", "aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI"},
	{"url password", "psql postgres://app:s3cretPw0rd@db.internal:5432/app", "s3cretPw0rd"},
	{"curl user", "curl -u admin:hunter22pw https://api.example.com", "hunter22pw"},
	{"secret variable", "PGPASSWORD=hunter22pw psql -h db", "hunter22pw"},
	{"private key", "printf '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU\n-----END OPENSSH PRIVATE KEY-----\n' > k", "b3BlbnNzaC1rZXktdjE"},
}

// TestCredentialFixturesRedacted is SP-014.1.a2: every credential fixture in
// a command is gone from what is stored, for a shell tool's command and for
// another tool's input.
func TestCredentialFixturesRedacted(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	for _, f := range credentialFixtures {
		w.Enqueue(key(session("creds")), 100, bashEvent(f.cmd))
		in, _ := json.Marshal(map[string]string{"url": "https://x", "prompt": f.cmd})
		w.Enqueue(key(session("creds")), 100, func() Record { return Record{Tool: "WebFetch", Command: Command("WebFetch", in)} })
	}
	flush(t, w)
	recs, err := s.Records(context.Background(), session("creds"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2*len(credentialFixtures) {
		t.Fatalf("%d records, want %d", len(recs), 2*len(credentialFixtures))
	}
	redacted := 0
	for i, f := range credentialFixtures {
		ok := true
		for _, r := range recs[2*i : 2*i+2] {
			if strings.Contains(r.Command, f.secret) || !strings.Contains(r.Command, "[REDACTED:") {
				t.Errorf("%s: stored %s command %q keeps the secret", f.name, r.Tool, r.Command)
				ok = false
			}
		}
		if ok {
			redacted++
		}
	}
	t.Logf("credential fixtures redacted: %d/%d (%.0f%%)", redacted, len(credentialFixtures),
		100*float64(redacted)/float64(len(credentialFixtures)))
}

// TestLongCommandTruncated is SP-014.1.a3: a command over 2 KB is stored
// cut to 2 KB with a marker that says how long it was.
func TestLongCommandTruncated(t *testing.T) {
	long := "echo " + strings.Repeat("x", 5000)
	in, _ := json.Marshal(map[string]string{"command": long})
	got := Command("Bash", in)
	if len(got) > MaxCommand {
		t.Errorf("len = %d, want <= %d", len(got), MaxCommand)
	}
	if !strings.HasPrefix(got, "echo xxx") || !strings.HasSuffix(got, fmt.Sprintf("…[truncated %d bytes]", len(long))) {
		t.Errorf("truncated command = ...%q", got[len(got)-40:])
	}
	if short := Command("Bash", []byte(`{"command":"ls -la"}`)); short != "ls -la" {
		t.Errorf("short command = %q, want unchanged", short)
	}
	// A cut never splits a character.
	multi := strings.Repeat("é", 3000)
	if c := Clean(multi, MaxCommand); !utf8.ValidString(c) || len(c) > MaxCommand {
		t.Errorf("multibyte cut: valid=%v len=%d", utf8.ValidString(c), len(c))
	}
	if c := truncate("abcdef", 3, 6); c != "…[truncated 6 bytes]" {
		t.Errorf("cap below the marker = %q", c)
	}
}

func TestCommandForms(t *testing.T) {
	tests := []struct {
		name, tool, in, want string
	}{
		{"bash string", "Bash", `{"command":"git status","description":"x"}`, "git status"},
		{"codex argv", "shell", `{"command":["bash","-lc","ls ~/code"]}`, "bash -lc ls ~/code"},
		{"exec_command cmd", "exec_command", `{"cmd":"make test"}`, "make test"},
		{"shell tool odd input", "Bash", `{"command":42}`, `{"command":42}`},
		{"not json", "Bash", `nope`, `nope`},
		{"file tool keeps input", "Read", `{"file_path":"/a/b"}`, `{"file_path":"/a/b"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Command(tt.tool, []byte(tt.in)); got != tt.want {
				t.Errorf("Command = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseOwner(t *testing.T) {
	tests := []struct {
		in      string
		want    Owner
		wantErr bool
	}{
		{"abc-123", session("abc-123"), false},
		{"session:abc", session("abc"), false},
		{"task:task-9", Owner{Kind: OwnerTask, ID: "task-9"}, false},
		{"pin:abc", Owner{}, true},
		{"", Owner{}, true},
		{"task:../x", Owner{}, true},
		{strings.Repeat("a", 129), Owner{}, true},
	}
	for _, tt := range tests {
		got, err := ParseOwner(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseOwner(%q) = %v, %v; want %v, err %v", tt.in, got, err, tt.want, tt.wantErr)
		}
		if err == nil {
			if back, _ := ParseOwner(got.String()); back != got {
				t.Errorf("ParseOwner(%q.String()) = %v", got, back)
			}
		}
	}
}

// writeChain stores n events and an end record for owner o.
func writeChain(t *testing.T, s *Store, o Owner, n int) {
	t.Helper()
	w := NewWriter(s, DefaultQueue, nil)
	defer w.Close()
	for i := range n {
		w.Enqueue(key(o), 100, bashEvent(fmt.Sprintf("cmd %d", i+1)))
	}
	ctx := context.Background()
	if err := w.End(ctx, o); err != nil {
		t.Fatalf("End: %v", err)
	}
}

func verify(t *testing.T, s *Store, o Owner) Report {
	t.Helper()
	rep, err := s.Verify(context.Background(), o)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

// TestVerifyDetectsTampering is SP-014.2.a1 at the store: a modified, a
// deleted and a reordered record are each detected; the CLI test runs the
// same cases through 'orch spaces log verify'.
func TestVerifyDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		tamper string
	}{
		{"modified", `UPDATE events SET command = 'rm -rf /' WHERE seq = 3`},
		{"modified decision", `UPDATE events SET decision = 'allow', rule = 'x' WHERE seq = 2`},
		{"deleted", `DELETE FROM events WHERE seq = 3`},
		{"deleted head", `DELETE FROM events WHERE seq = 6`},
		{"deleted run", `DELETE FROM events WHERE seq IN (2, 3)`},
		{"reordered", `UPDATE events SET seq = -seq WHERE seq IN (2, 4);
			UPDATE events SET seq = 4 WHERE seq = -2; UPDATE events SET seq = 2 WHERE seq = -4`},
		{"reordered with hashes", `UPDATE events SET seq = -seq WHERE seq IN (2, 3);
			UPDATE events SET seq = 3 WHERE seq = -2; UPDATE events SET seq = 2 WHERE seq = -3`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTemp(t)
			o := session("tamper")
			writeChain(t, s, o, 5)
			if rep := verify(t, s, o); !rep.OK() || rep.Records != 6 || rep.Anchors != 1 || rep.Ends != 1 {
				t.Fatalf("untouched chain: %+v", rep)
			}
			if _, err := s.db.Exec(c.tamper); err != nil {
				t.Fatal(err)
			}
			rep := verify(t, s, o)
			if rep.OK() {
				t.Fatalf("tampering not detected: %+v", rep)
			}
			t.Logf("%s: %+v", c.name, rep.Problems)
		})
	}
}

func TestVerifyOtherOwnersUntouched(t *testing.T) {
	s, _ := openTemp(t)
	writeChain(t, s, session("one"), 3)
	writeChain(t, s, Owner{Kind: OwnerTask, ID: "one"}, 3)
	if _, err := s.db.Exec(`DELETE FROM events WHERE owner_kind = 'task' AND seq = 2`); err != nil {
		t.Fatal(err)
	}
	if rep := verify(t, s, session("one")); !rep.OK() {
		t.Errorf("session chain: %+v", rep)
	}
	if rep := verify(t, s, Owner{Kind: OwnerTask, ID: "one"}); rep.OK() {
		t.Error("task chain: deletion not detected")
	}
	if rep := verify(t, s, session("none")); !rep.OK() || rep.Records != 0 {
		t.Errorf("empty chain: %+v", rep)
	}
}

func TestChainContinuesAcrossWritersAndEnds(t *testing.T) {
	s, _ := openTemp(t)
	o := session("resumed")
	writeChain(t, s, o, 2)
	// A new orchd, and a resumed session after its SessionEnd.
	writeChain(t, s, o, 2)
	rep := verify(t, s, o)
	if !rep.OK() || rep.Records != 6 || rep.Anchors != 2 || rep.Ends != 2 {
		t.Errorf("report = %+v", rep)
	}
}

// TestDroppedEventsLeaveGap is D-30 and the audit's H2: an owner that
// floods the log drops only its own events once it holds its share of the
// queue; dropped events are counted and stored as a gap record just before
// the owner's next event, or after the batch when none follows.
func TestDroppedEventsLeaveGap(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, 8) // a share of 2 events per owner
	release := make(chan struct{})
	started := make(chan struct{})
	o, other := session("flood"), session("quiet")
	w.Enqueue(key(o), 100, func() Record {
		close(started)
		<-release
		return Record{Command: "first"}
	})
	<-started
	if !w.Enqueue(key(o), 100, bashEvent("second")) {
		t.Fatal("second event should fit the owner's share")
	}
	for range 3 {
		if w.Enqueue(key(o), 100, bashEvent("lost")) {
			t.Fatal("an event past the owner's share was queued")
		}
	}
	// Another session still has room while the first floods.
	for _, c := range []string{"a", "b"} {
		if !w.Enqueue(key(other), 100, bashEvent(c)) {
			t.Fatalf("quiet owner's %s was dropped", c)
		}
	}
	if w.Enqueue(key(other), 100, bashEvent("c")) {
		t.Fatal("quiet owner's third event exceeded its share")
	}
	close(release)
	flush(t, w)
	w.Enqueue(key(o), 100, bashEvent("third"))
	w.Enqueue(key(o), 100, bashEvent("fourth"))
	flush(t, w)
	if w.Dropped() != 4 {
		t.Fatalf("Dropped() = %d, want 4", w.Dropped())
	}
	chain := func(o Owner) string {
		recs, err := s.Records(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, r := range recs {
			kinds = append(kinds, fmt.Sprintf("%s:%s:%d", r.Kind, r.Command, r.Dropped))
		}
		return strings.Join(kinds, " ")
	}
	if got, want := chain(o), "event:first:0 event:second:0 gap::3 event:third:0 event:fourth:0"; got != want {
		t.Errorf("flood chain = %s, want %s", got, want)
	}
	if got, want := chain(other), "event:a:0 event:b:0 gap::1"; got != want {
		t.Errorf("quiet chain = %s, want %s", got, want)
	}
	for _, x := range []Owner{o, other} {
		if rep := verify(t, s, x); !rep.OK() {
			t.Errorf("%s: %+v", x, rep)
		}
	}
}

// TestByteBudget: events past the queue's byte budget, or past one owner's
// share of it, are dropped however few they are.
func TestByteBudget(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	release := make(chan struct{})
	started := make(chan struct{})
	big := session("big")
	w.Enqueue(key(big), MaxQueuedBytes/ownerShare, func() Record { close(started); <-release; return Record{} })
	<-started
	if w.Enqueue(key(big), 1, bashEvent("over its share")) {
		t.Error("an owner exceeded its byte share")
	}
	for i := range ownerShare - 1 {
		if !w.Enqueue(key(session(fmt.Sprint("other", i))), MaxQueuedBytes/ownerShare, bashEvent("fits")) {
			t.Fatalf("owner %d could not use its share", i)
		}
	}
	if w.Enqueue(key(session("late")), 1, bashEvent("over budget")) {
		t.Error("an event exceeded the total byte budget")
	}
	close(release)
	flush(t, w)
	if !w.Enqueue(key(session("late")), 1, bashEvent("fits again")) {
		t.Error("the budget was not released after storing")
	}
	flush(t, w)
	if rep := verify(t, s, session("late")); rep.Records != 2 || rep.Dropped != 1 {
		t.Errorf("late chain = %+v, want a gap then an event", rep)
	}
}

// TestEndCarriesGap: events dropped before SessionEnd show before its end
// record, so the anchored head comes last.
func TestEndCarriesGap(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, 4) // a share of 1
	release := make(chan struct{})
	started := make(chan struct{})
	o := session("ending")
	w.Enqueue(key(o), 100, func() Record { close(started); <-release; return Record{Command: "slow"} })
	<-started
	w.Enqueue(key(o), 100, bashEvent("dropped"))
	close(release)
	if err := w.End(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[1].Kind != KindGap || recs[2].Kind != KindEnd {
		t.Errorf("records = %+v", recs)
	}
}

func TestBuildPanicStoresPlaceholder(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	o := session("panicky")
	w.Enqueue(key(o), 100, func() Record { panic("boom") })
	w.Enqueue(key(o), 100, bashEvent("after"))
	flush(t, w)
	recs, err := s.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || !strings.Contains(recs[0].Command, "could not be built") || recs[1].Command != "after" {
		t.Errorf("records = %+v", recs)
	}
}

func TestWriterClosed(t *testing.T) {
	s, _ := openTemp(t)
	w := NewWriter(s, DefaultQueue, nil)
	o := session("closing")
	w.Enqueue(key(o), 100, bashEvent("queued before close"))
	w.Close()
	w.Close()
	if rep := verify(t, s, o); rep.Records != 1 {
		t.Errorf("Close did not store the queued event: %+v", rep)
	}
	if w.Enqueue(key(o), 100, bashEvent("late")) {
		t.Error("Enqueue after Close succeeded")
	}
	ctx := context.Background()
	if err := w.Flush(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close = %v", err)
	}
	if err := w.End(ctx, o); !errors.Is(err, ErrClosed) {
		t.Errorf("End after Close = %v", err)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.Enqueue(key(session("slow")), 100, func() Record { close(started); <-release; return Record{} })
	<-started
	if !w.Enqueue(key(session("other")), 100, bashEvent("fills the queue")) {
		t.Fatal("queue should have room for one more")
	}
	o := session("waiting")
	if w.Enqueue(key(o), 100, bashEvent("dropped")) {
		t.Fatal("queue should be full")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Flush = %v, want deadline exceeded", err)
	}
	// An End that could not be queued gives its gap back.
	if err := w.End(ctx, o); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("End = %v, want deadline exceeded", err)
	}
	w.mu.Lock()
	kept := w.drops[dropKey{owner: o}]
	w.mu.Unlock()
	if kept != 1 {
		t.Errorf("drops kept for %s = %d, want 1", o, kept)
	}
}

// TestFailedBatchCountsAsDropped: when the store cannot write, the batch is
// counted as dropped, so its loss shows as a gap once writing works again.
func TestFailedBatchCountsAsDropped(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	if _, err := s.db.Exec(`DROP TABLE events`); err != nil {
		t.Fatal(err)
	}
	w.Enqueue(key(session("broken")), 100, bashEvent("lost"))
	if err := w.Flush(context.Background()); err == nil {
		// Flush may share the failed batch or follow it.
		flush(t, w)
	}
	if w.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", w.Dropped())
	}
	if _, err := s.db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	w.Enqueue(key(session("broken")), 100, bashEvent("kept"))
	flush(t, w)
	rep := verify(t, s, session("broken"))
	if !rep.OK() || rep.Gaps != 1 || rep.Dropped != 1 || rep.Records != 2 {
		t.Errorf("after recovery: %+v", rep)
	}
}

// TestAppendThroughput is SP-014.2.a2 for the store alone; the daemon test
// TestToolLogThroughput measures it with targets read for each event.
func TestAppendThroughput(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	const n = 5000
	start := time.Now()
	for i := range n {
		for !w.Enqueue(key(session(fmt.Sprintf("s%d", i%8))), 100, bashEvent(fmt.Sprintf("git -C /w/r%d status --short", i))) {
			time.Sleep(time.Millisecond)
		}
	}
	flush(t, w)
	rate := float64(n) / time.Since(start).Seconds()
	t.Logf("append throughput: %.0f events/s (%d events)", rate, n)
	if rate < 1000 && !raceEnabled {
		t.Errorf("throughput %.0f events/s, want >= 1000", rate)
	}
	var events int
	for i := range 8 {
		recs, err := s.Records(context.Background(), session(fmt.Sprintf("s%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Kind == KindEvent {
				events++
			}
		}
	}
	if events != n {
		t.Errorf("stored %d events, want %d", events, n)
	}
}

// TestCleanLargeAndInvalid: a huge field is redacted only near what is kept
// and still says how long it was, and invalid UTF-8 is stored as U+FFFD so
// what is hashed is what is stored (audit L4).
func TestCleanLargeAndInvalid(t *testing.T) {
	huge := "ghp_R4nd0mT0kenValue1234567890abcdEFGH " + strings.Repeat("y", 1<<20)
	got := Clean(huge, MaxCommand)
	if len(got) > MaxCommand || strings.Contains(got, "ghp_") ||
		!strings.HasSuffix(got, fmt.Sprintf("…[truncated %d bytes]", len(huge))) {
		t.Errorf("huge field = %q...", got[:80])
	}
	// A secret straddling the cap is still found.
	edge := strings.Repeat("x", MaxCommand-20) + " sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	if c := Clean(edge, MaxCommand); strings.Contains(c, "sk-ant-api03-AbCd") {
		t.Errorf("secret at the cut kept: %q", c[len(c)-80:])
	}
	// A key the pre-cap cut before its END line is still removed.
	cut := strings.Repeat("x", 1000) + "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3BlbnNz", 1<<12) +
		"\n-----END OPENSSH PRIVATE KEY-----"
	if c := Clean(cut, MaxCommand); strings.Contains(c, "b3BlbnNz") {
		t.Errorf("cut private key kept: %q", c)
	}
	if c := Clean("a\xffb\xfec", MaxField); c != "a\uFFFDb\uFFFDc" {
		t.Errorf("invalid UTF-8 = %q", c)
	}
}

// TestResolverRoutesClaimsAndGaps is the round-2 audit's M-a: an event whose
// hook named another session goes where the resolver says, and so does the
// gap of such events that were dropped.
func TestResolverRoutesClaimsAndGaps(t *testing.T) {
	s, _ := openTemp(t)
	pin, earlier := session("current"), session("earlier")
	resolve := func(k Key) Owner {
		switch k.Claim {
		case "earlier":
			return earlier
		case "task-kind":
			return Owner{Kind: OwnerTask, ID: "x"} // another kind is ignored
		}
		return k.Owner
	}
	w := NewWriter(s, 8, resolve) // a share of 2 per quota key
	t.Cleanup(w.Close)
	release := make(chan struct{})
	started := make(chan struct{})
	q := Key{Quota: "process:1", Owner: pin, Claim: "earlier"}
	w.Enqueue(q, 10, func() Record { close(started); <-release; return Record{Command: "blocked"} })
	<-started
	w.Enqueue(q, 10, bashEvent("queued"))
	if w.Enqueue(q, 10, bashEvent("dropped")) {
		t.Fatal("a third event fit a share of 2")
	}
	// Another session of the same process shares the process's quota.
	if w.Enqueue(Key{Quota: "process:1", Owner: session("minted")}, 10, bashEvent("x")) {
		t.Error("a new session of the same process got its own share")
	}
	close(release)
	flush(t, w)
	w.Enqueue(Key{Quota: "process:1", Owner: pin, Claim: "task-kind"}, 10, bashEvent("other kind"))
	w.Enqueue(Key{Quota: "process:1", Owner: pin, Claim: "current"}, 10, bashEvent("own id"))
	flush(t, w)
	chain := func(o Owner) []string {
		recs, err := s.Records(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range recs {
			out = append(out, fmt.Sprintf("%s:%s:%d", r.Kind, r.Command, r.Dropped))
		}
		return out
	}
	if got := strings.Join(chain(earlier), " "); got != "event:blocked:0 event:queued:0 gap::1" {
		t.Errorf("earlier chain = %s", got)
	}
	if got := strings.Join(chain(pin), " "); got != "event:other kind:0 event:own id:0" {
		t.Errorf("current chain = %s", got)
	}
	if got := strings.Join(chain(session("minted")), " "); got != "gap::1" {
		t.Errorf("minted chain = %s", got)
	}
}

// TestEndOnlyAfterNewRecords: a repeated End adds nothing, a chain with no
// records gets no end record, and records after an end are anchored at the
// next End (a resumed session).
func TestEndOnlyAfterNewRecords(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	ctx := context.Background()
	o := session("resumed")
	if err := w.End(ctx, session("empty")); err != nil {
		t.Fatal(err)
	}
	w.Enqueue(key(o), 10, bashEvent("one"))
	for range 3 {
		if err := w.End(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	w.Enqueue(key(o), 10, bashEvent("after resume"))
	if err := w.End(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := w.End(ctx, o); err != nil {
		t.Fatal(err)
	}
	rep := verify(t, s, o)
	if !rep.OK() || rep.Records != 4 || rep.Ends != 2 || rep.Anchors != 2 {
		t.Errorf("resumed chain = %+v, want event, end, event, end", rep)
	}
	if rep := verify(t, s, session("empty")); rep.Records != 0 {
		t.Errorf("empty chain = %+v", rep)
	}
}

// TestDropKeysBounded: a hook that names a new session for every dropped
// event cannot grow the drop counters past maxDropKeys; the overflow is
// counted under the caller's own chain.
func TestDropKeysBounded(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	t.Cleanup(func() { close(release) })
	o := session("flood")
	w.Enqueue(key(o), 10, func() Record { close(started); <-release; return Record{} })
	<-started
	w.Enqueue(Key{Quota: "q", Owner: session("other")}, 10, bashEvent("fills the queue"))
	for i := range maxDropKeys + 500 {
		w.Enqueue(Key{Quota: "flood", Owner: o, Claim: fmt.Sprint("claim-", i)}, 10, bashEvent("x"))
	}
	w.mu.Lock()
	n, own := len(w.drops), w.drops[dropKey{owner: o}]
	w.mu.Unlock()
	if n > maxDropKeys+1 || own != 500 {
		t.Errorf("%d drop counters, %d under the caller's chain; want <= %d and 500", n, own, maxDropKeys+1)
	}
}
