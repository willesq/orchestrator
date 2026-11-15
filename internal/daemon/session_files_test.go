package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// writeEvent is a Write tool call putting text at path, as the hook reports it.
func writeEvent(c resolvedCaller, path, text string, refused bool) toolEvent {
	in, _ := json.Marshal(map[string]string{"file_path": path, "content": text})
	req := &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", SessionId: "s", ToolName: "Write", ToolInput: in, Cwd: filepath.Dir(path)}
	if refused {
		req.GuardVerdict, req.GuardRule, req.GuardEnforced = "refuse", "other_job", true
	}
	return toolEvent{caller: c, req: req, received: time.Now()}
}

// handedOff runs the edit hand-off of path and returns the write it stored,
// placed as orchd places it.
func handedOff(t *testing.T, e readEnv, path string) rag.Meta {
	t.Helper()
	ing := newRecordingIngester()
	if err := storeFile(context.Background(), ing, handoff.File{Path: path}, "/home/u", nil); err != nil {
		t.Fatal(err)
	}
	m := <-ing.metas
	<-ing.texts
	placer := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos, files: e.s.files}
	if err := placer.place(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestFileWrittenOutsideRootsKeepsItsSpace is SP-029.1.a3: a file a session
// in acme writes in a temporary folder (under no root) and that is later
// handed off and ingested is placed in acme. The record is matched on the
// text: a session of another space that only names the path (a forged or
// earlier claim), or a file changed since, stays in quarantine; the same
// text written by two spaces goes to quarantine; a call the guard refused
// records nothing; a file under a root is placed by its root.
func TestFileWrittenOutsideRootsKeepsItsSpace(t *testing.T) {
	e := newReadEnv(t)
	e.s.files = newSessionFiles(e.s.pins.db)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acme := resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-acme", Space: "acme"}}
	globex := resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-globex", Space: "globex"}}
	globexTask := resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-g", Space: "globex"}}
	write := func(c resolvedCaller, name, text string) string {
		p := filepath.Join(tmp, name)
		e.s.toolRecord(writeEvent(c, p, text, false), "claude")
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	notes := write(acme, "notes.md", "# notes\nacme's plan")
	if m := handedOff(t, e, notes); m.Space != "acme" || m.ContentHash == "" {
		t.Fatalf("a file acme's session wrote in a temporary folder was placed in %q", m.Space)
	}
	// globex names the same path with other text: acme's text stays acme's.
	e.s.toolRecord(writeEvent(globex, notes, "globex claims it", false), "claude")
	if m := handedOff(t, e, notes); m.Space != "acme" {
		t.Fatalf("after another space's claim the file was placed in %q", m.Space)
	}
	// A claim made first, for text the claimant did not write, places nothing.
	claimed := filepath.Join(tmp, "claimed.md")
	e.s.toolRecord(writeEvent(globex, claimed, "a guess", false), "claude")
	if err := os.WriteFile(claimed, []byte("# acme's report, written by a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := handedOff(t, e, claimed); m.Space != spaces.Quarantine {
		t.Fatalf("a file another space only claimed was placed in %q", m.Space)
	}
	// Changed since the recorded write (an Edit, a program): not proven.
	if err := os.WriteFile(notes, []byte("# notes\nedited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := handedOff(t, e, notes); m.Space != spaces.Quarantine {
		t.Fatalf("a changed file was placed in %q", m.Space)
	}

	shared := write(acme, "shared.md", "same text")
	write(globexTask, "shared.md", "same text")
	if m := handedOff(t, e, shared); m.Space != spaces.Quarantine {
		t.Fatalf("a text acme and globex both wrote was placed in %q", m.Space)
	}

	refused := filepath.Join(tmp, "refused.md")
	e.s.toolRecord(writeEvent(acme, refused, "x", true), "claude")
	if sps, _ := e.s.files.spacesOf(context.Background(), refused, writeHash("x")); len(sps) != 0 {
		t.Fatalf("a refused write was recorded: %v", sps)
	}

	// A file under a root is placed by its root, and its writes are not
	// recorded: a session in acme writing in globex's folder does not
	// move it into acme.
	inGlobex := filepath.Join(e.code, "globex", "repo", "plan.md")
	e.s.toolRecord(writeEvent(acme, inGlobex, "# plan", false), "claude")
	if err := os.WriteFile(inGlobex, []byte("# plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sps, _ := e.s.files.spacesOf(context.Background(), inGlobex, writeHash("# plan")); len(sps) != 0 {
		t.Fatalf("a write under a root was recorded: %v", sps)
	}
	if m := handedOff(t, e, inGlobex); m.Space != "globex" {
		t.Fatalf("a file in globex's folder was placed in %q", m.Space)
	}

	// Callers with no space record nothing: the operator, an untracked
	// task, an unidentified caller, a session pinned to a public space or
	// to quarantine, an untrusted pin under a task's session ID.
	other := filepath.Join(tmp, "other.md")
	for _, c := range []resolvedCaller{
		{kind: callerOperator, operatorBy: operatorApp}, {kind: callerTask}, none("x"),
		{kind: callerSession, pin: sessionPin{SessionID: "sess-oss", Space: "oss"}},
		{kind: callerSession, pin: sessionPin{SessionID: "sess-q", Space: spaces.Quarantine}},
		{kind: callerSession, pin: sessionPin{SessionID: agentrun.SessionID("task-x"), Space: "acme"}},
	} {
		e.s.toolRecord(writeEvent(c, other, "o", false), "claude")
	}
	if sps, _ := e.s.files.spacesOf(context.Background(), other, writeHash("o")); len(sps) != 0 {
		t.Fatalf("callers with no space recorded %v", sps)
	}

	// Only a Write's whole text is a proof: other calls record nothing.
	if _, _, ok := writtenText("Edit", []byte(`{"file_path":"/x","new_string":"y"}`), []string{"/x"}); ok {
		t.Fatal("an Edit was taken for a whole write")
	}
	if _, _, ok := writtenText("Write", []byte(`{"file_path":"/x"}`), []string{"/x"}); ok {
		t.Fatal("a Write with no content was taken")
	}
	if _, _, ok := writtenText("Write", []byte(`{"content":"y"}`), nil); ok {
		t.Fatal("a Write with no target was taken")
	}

	// A space the applied policy no longer allows places nothing.
	if _, err := e.s.pins.db.Exec(`UPDATE session_files SET space = 'oss' WHERE path = ?`, fileKey(write(acme, "moved.md", "m"))); err != nil {
		t.Fatal(err)
	}
	if m := handedOff(t, e, filepath.Join(tmp, "moved.md")); m.Space != spaces.Quarantine {
		t.Fatalf("a record naming a public space placed the file in %q", m.Space)
	}
}

// TestSessionFilesExpire: a write older than sessionFilesKeep no longer
// places a file and is pruned, the table keeps at most sessionFilesMax
// rows (the newest), and with no record, no policy or a failing store
// nothing is placed.
func TestSessionFilesExpire(t *testing.T) {
	e := newReadEnv(t)
	f := newSessionFiles(e.s.pins.db)
	res, _, err := e.s.spaces.CurrentRevision()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f.now = func() time.Time { return start }
	f.record(context.Background(), res, "acme", "session:a", "/scratch/old.md", "old")
	f.record(context.Background(), res, "acme", "session:a", "relative.md", "r")
	f.now = func() time.Time { return start.Add(time.Hour) }
	f.record(context.Background(), res, "globex", "session:g", "/scratch/new.md", "new")
	if sps, _ := f.spacesOf(context.Background(), "relative.md", writeHash("r")); len(sps) != 0 {
		t.Fatalf("a relative path was recorded: %v", sps)
	}
	if sps, _ := f.spacesOf(context.Background(), "/SCRATCH/new.md", writeHash("new")); len(sps) != 1 {
		t.Fatalf("lookup is not case-insensitive: %v", sps)
	}

	prevMax := sessionFilesMax
	sessionFilesMax = 1
	t.Cleanup(func() { sessionFilesMax = prevMax })
	f.prune(context.Background())
	if sps, _ := f.spacesOf(context.Background(), "/scratch/old.md", writeHash("old")); len(sps) != 0 {
		t.Fatalf("the oldest row beyond the cap was kept: %v", sps)
	}
	if sps, _ := f.spacesOf(context.Background(), "/scratch/new.md", writeHash("new")); len(sps) != 1 {
		t.Fatalf("the newest row was dropped: %v", sps)
	}

	f.now = func() time.Time { return start.Add(sessionFilesKeep + 2*time.Hour) }
	if sps, _ := f.spacesOf(context.Background(), "/scratch/new.md", writeHash("new")); len(sps) != 0 {
		t.Fatalf("an expired write still places: %v", sps)
	}
	f.prune(context.Background())
	var n int
	if err := e.s.pins.db.QueryRow(`SELECT COUNT(*) FROM session_files`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after expiry = %d, %v", n, err)
	}

	// Nil, no policy, no hash and a failing store record and place nothing.
	var nilFiles *sessionFiles
	nilFiles.record(context.Background(), res, "acme", "session:a", "/x", "x")
	nilFiles.prune(context.Background())
	if sps, err := nilFiles.spacesOf(context.Background(), "/x", "h"); sps != nil || err != nil {
		t.Fatal("nil record answered")
	}
	if sps, err := f.spacesOf(context.Background(), "/x", ""); sps != nil || err != nil {
		t.Fatal("a write with no hash matched")
	}
	f.record(context.Background(), nil, "acme", "session:a", "/scratch/nopolicy.md", "n")
	if newSessionFiles(nil) != nil {
		t.Fatal("a record with no store")
	}
	_ = e.s.pins.db.Close()
	f.record(context.Background(), res, "acme", "session:a", "/scratch/closed.md", "c")
	f.prune(context.Background())
	if _, err := f.spacesOf(context.Background(), "/scratch/closed.md", writeHash("c")); err == nil {
		t.Fatal("a closed store answered")
	}
	w := &writePlacer{files: f}
	m := &rag.Meta{ContentHash: writeHash("c"), Origin: rag.Origin{System: rag.OriginFS, Path: "/scratch/closed.md"}}
	if _, _, err := w.writtenPlacement(context.Background(), res, m); err == nil {
		t.Fatal("placement ignored a failing store")
	}
}
