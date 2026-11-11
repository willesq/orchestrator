package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/queue"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
	"github.com/willesq/orchestrator/internal/workflows"
)

// withCopyingToolLog attaches a command log that copies its anchors into
// s's orch.db, the way Run does, and returns its path.
func withCopyingToolLog(t *testing.T, s *server) string {
	t.Helper()
	path := t.TempDir() + "/toollog.db"
	l, err := openToolLogCopying(path, s.resolveClaim, s.pins.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.toolLog = l
	s.toolEvents.setSink(s.logToolEvent)
	return path
}

// copiesOf reads o's anchors from orch.db.
func copiesOf(t *testing.T, s *server, o toollog.Owner) []toollog.Anchor {
	t.Helper()
	c, err := s.toolLog.copies.of(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return c.Anchors
}

// waitAnchors waits until o's chain has n anchors in toollog.db.
func waitAnchors(t *testing.T, s *server, o toollog.Owner, n int) toollog.Report {
	t.Helper()
	var rep toollog.Report
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		if rep, err = s.toolLog.store.Verify(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if rep.Anchors >= n {
			return rep
		}
	}
	t.Fatalf("%s has %d anchor(s), want %d", o, rep.Anchors, n)
	return rep
}

func problemsText(rep *orchdv1.VerifyToolLogResponse) string {
	var b strings.Builder
	for _, p := range rep.GetProblems() {
		fmt.Fprintf(&b, "%d: %s\n", p.GetSeq(), p.GetDetail())
	}
	return b.String()
}

// TestAnchorsCopiedIntoOrchDB is SP-073.1.a1 through orchd: each anchor a
// SessionEnd writes is also in orch.db; VerifyToolLog reports a chain cut
// after its last anchor in the file, an anchor only in toollog.db and one
// only in orch.db.
func TestAnchorsCopiedIntoOrchDB(t *testing.T) {
	s, dirs := pinsServer(t)
	path := withCopyingToolLog(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-copy", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	o := sessionOwner("sess-copy")
	ctx := pinnedCtx(t, s, "sess-copy")
	for run := range 2 {
		for i := range 3 {
			if _, err := s.RecordToolEvent(ctx, bashReq("sess-copy", dirs[0], fmt.Sprint("echo ", run, i))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-copy"}); err != nil {
			t.Fatal(err)
		}
		waitAnchors(t, s, o, run+1)
	}
	inFile, err := s.toolLog.store.Anchors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	copies := copiesOf(t, s, o)
	if len(copies) != 2 || len(inFile) != 2 || copies[0] != inFile[0] || copies[1] != inFile[1] {
		t.Fatalf("orch.db copies %+v, file anchors %+v; want the same two", copies, inFile)
	}
	verifyOwner := func() *orchdv1.VerifyToolLogResponse {
		t.Helper()
		rep, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-copy"})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := verifyOwner(); len(rep.GetProblems()) != 0 || rep.GetAnchors() != 2 {
		t.Fatalf("intact chain: %v", rep)
	}

	// An anchor only in toollog.db.
	if _, err := s.pins.db.Exec(`DELETE FROM toollog_anchors WHERE seq = 4`); err != nil {
		t.Fatal(err)
	}
	if got := problemsText(verifyOwner()); !strings.Contains(got, "4: anchored in toollog.db but orch.db has no copy") {
		t.Fatalf("anchor missing from orch.db: %q", got)
	}
	s.toolLog.copies.addAnchors([]toollog.Anchor{copies[0]})

	// Records after the last anchor in the file removed, with that anchor:
	// the file alone verifies, orch.db's copy does not.
	if err := tamperFile(path, `DELETE FROM events WHERE seq > 4; DELETE FROM anchors WHERE seq > 4`); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.toolLog.store.Verify(context.Background(), o); err != nil || !rep.OK() {
		t.Fatalf("the cut file alone: %+v, %v; want it to verify", rep, err)
	}
	got := problemsText(verifyOwner())
	for _, want := range []string{"8: anchored in orch.db but the anchor is missing from toollog.db",
		"8: the anchored chain head is missing (records after it were removed)"} {
		if !strings.Contains(got, want) {
			t.Errorf("cut chain: %q; want %q", got, want)
		}
	}
}

// TestCopiesAtOpen: when orchd opens the log, anchors toollog.db holds that
// orch.db lacks (a crash fell between the two writes, or the log predates
// SP-073) are copied and copies orch.db holds are never replaced; pruned
// heads are copied the first time only, so one written into the file later
// is reported, not copied.
func TestCopiesAtOpen(t *testing.T) {
	s, dirs := pinsServer(t)
	path := withToolLog(t, s) // no copies, as before SP-073
	if _, err := s.PinSession(inClaude, pinReq("sess-old", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	o := sessionOwner("sess-old")
	for _, cmd := range []string{"ls", "pwd"} {
		if _, err := s.RecordToolEvent(pinnedCtx(t, s, "sess-old"), bashReq("sess-old", dirs[0], cmd)); err != nil {
			t.Fatal(err)
		}
	}
	s.endToolLog("sess-old")
	waitAnchors(t, s, o, 1)
	// A cut retention made before SP-073.
	if err := tamperFile(path, `INSERT INTO pruned (owner_kind, owner_id, seq, hash, kind)
		SELECT owner_kind, owner_id, seq, hash, kind FROM events WHERE seq = 1; DELETE FROM events WHERE seq = 1`); err != nil {
		t.Fatal(err)
	}
	_ = s.toolLog.Close()
	reopen := func() {
		t.Helper()
		l, err := openToolLogCopying(path, s.resolveClaim, s.pins.db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		s.toolLog = l
	}
	reopen()
	if c := copiesOf(t, s, o); len(c) != 1 || c[0].Seq != 3 {
		t.Fatalf("copies after the first open = %+v", c)
	}
	verify := func() string {
		t.Helper()
		rep, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-old"})
		if err != nil {
			t.Fatal(err)
		}
		return problemsText(rep)
	}
	if got := verify(); got != "" {
		t.Fatalf("after the first open: %q", got)
	}
	// A copy that disagrees is kept; a missing one is copied back; a
	// pruned head written into the file now is not copied.
	if _, err := s.pins.db.Exec(`UPDATE toollog_anchors SET hash = 'kept'`); err != nil {
		t.Fatal(err)
	}
	if err := tamperFile(path, `UPDATE pruned SET seq = 2, hash = (SELECT hash FROM events WHERE seq = 2); DELETE FROM events WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	_ = s.toolLog.Close()
	reopen()
	if c := copiesOf(t, s, o); len(c) != 1 || c[0].Hash != "kept" {
		t.Fatalf("copies after a second open = %+v; want the copy kept", c)
	}
	got := verify()
	for _, want := range []string{"3: the anchors in toollog.db and orch.db disagree", "does not match orch.db's copy (record 1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("after the second open: %q; want %q", got, want)
		}
	}
	if _, err := s.pins.db.Exec(`DELETE FROM toollog_anchors`); err != nil {
		t.Fatal(err)
	}
	_ = s.toolLog.Close()
	reopen()
	if c := copiesOf(t, s, o); len(c) != 1 || c[0].Seq != 3 || c[0].Hash == "kept" {
		t.Fatalf("copies after a third open = %+v; want the file's copied back", c)
	}
}

// TestCopiesRetried: copies orch.db cannot take wait and are written by a
// later flush; past the bound the oldest are dropped; a closed log writes
// what waits.
func TestCopiesRetried(t *testing.T) {
	s, _ := pinsServer(t)
	withCopyingToolLog(t, s)
	c := s.toolLog.copies
	a := toollog.Anchor{Owner: sessionOwner("sess-r"), Seq: 3, Hash: "h", At: 1}
	k := toollog.Cut{Owner: sessionOwner("sess-r"), Seq: 2, Hash: "c"}
	if _, err := s.pins.db.Exec(`ALTER TABLE toollog_cuts RENAME TO held`); err != nil {
		t.Fatal(err)
	}
	c.addAnchors([]toollog.Anchor{a})
	c.addCut(k)
	c.flush()
	c.mu.Lock()
	waiting := len(c.anchors) + len(c.cuts)
	c.mu.Unlock()
	if waiting != 2 {
		t.Fatalf("%d copies wait; want the anchor and the cut", waiting)
	}
	c.mu.Lock()
	c.anchors = bounded(append(c.anchors, make([]toollog.Anchor, maxPendingCopies)...), "anchor")
	n, first := len(c.anchors), c.anchors[0]
	c.anchors = []toollog.Anchor{a}
	c.mu.Unlock()
	if n != maxPendingCopies || first == a {
		t.Fatalf("bounded kept %d, first %+v; want the newest %d", n, first, maxPendingCopies)
	}
	if _, err := s.pins.db.Exec(`ALTER TABLE held RENAME TO toollog_cuts`); err != nil {
		t.Fatal(err)
	}
	s.sweepToolLog(context.Background(), time.Now())
	got, err := c.of(context.Background(), a.Owner)
	if err != nil || len(got.Anchors) != 1 || got.Anchors[0] != a || got.Cut.Seq != 2 || got.Cut.Hash != "c" {
		t.Fatalf("after the retry: %+v, %v", got, err)
	}
	// A cut only moves forward.
	c.addCut(toollog.Cut{Owner: k.Owner, Seq: 1, Hash: "older"})
	_ = s.toolLog.Close()
	if got, _ := c.of(context.Background(), a.Owner); got.Cut.Seq != 2 {
		t.Fatalf("an older cut moved the copy back: %+v", got.Cut)
	}
	// A cut handed over after the copier stopped (a sweep outliving
	// shutdown) is written at once.
	c.addCut(toollog.Cut{Owner: k.Owner, Seq: 5, Hash: "late"})
	c.mu.Lock()
	waiting = len(c.cuts)
	c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d cut(s) wait after the copier stopped", waiting)
	}
	if got, _ := c.of(context.Background(), a.Owner); got.Cut.Seq != 5 {
		t.Fatalf("the late cut was not written: %+v", got.Cut)
	}
}

// taskEnv is a server with a command log, a real task ledger in its
// orch.db wired to the task hub as Run wires it, and a fake runner.
type taskEnv struct {
	s     *server
	tasks *queue.SQLTaskStore
	// running holds the IDs of tasks the fake runner runs; the anchorer
	// reads it from its own goroutine.
	running sync.Map
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	s, _ := pinsServer(t)
	withCopyingToolLog(t, s)
	e := &taskEnv{s: s, tasks: queue.NewSQLTaskStore(s.pins.db)}
	s.hub = newTaskHub()
	e.tasks.OnChange = s.hub.publish
	s.toolLog.taskStatus = e.tasks.Status
	s.toolLog.running = func(id string) bool { _, ok := e.running.Load(id); return ok }
	return e
}

// call is one tool call of task id, as its agent's hook reports it.
func (e *taskEnv) call(t *testing.T, id, cmd string) {
	t.Helper()
	ctx := context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: id}})
	if _, err := e.s.RecordToolEvent(ctx, bashReq("whatever", "/w", cmd)); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)
}

// TestTaskChainAnchoredAtTerminalState is SP-073.1.a2: a queued task's chain
// gets an end record and an anchor (also in orch.db) when the task becomes
// complete, failed, timed out or cancelled; a retried task writes on in the
// same chain and is anchored again at its next end.
func TestTaskChainAnchoredAtTerminalState(t *testing.T) {
	e := newTaskEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { e.s.runTaskAnchors(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// The anchorer subscribes before it reads; wait for it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.s.hub.mu.Lock()
		n := len(e.s.hub.subs)
		e.s.hub.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task anchorer never subscribed")
		}
	}

	ends := map[string]func(id string) error{
		"complete": func(id string) error {
			return e.tasks.SaveResult(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}, workflows.Outcome{Status: "complete"})
		},
		"failed": func(id string) error {
			return e.tasks.SaveResult(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}, workflows.Outcome{Status: "failed", Result: "boom"})
		},
		"timed-out": func(id string) error {
			return e.tasks.SaveResult(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}, workflows.Outcome{Status: "failed", Result: "timed out after 30m0s"})
		},
		"cancelled": func(id string) error { return e.tasks.End(ctx, id, queue.StatusCancelled, queue.CancelledByUser) },
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			id := "task-" + name
			o := toollog.Owner{Kind: toollog.OwnerTask, ID: id}
			if err := e.tasks.Create(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev", Body: "b"}); err != nil {
				t.Fatal(err)
			}
			if err := e.tasks.SetStatus(ctx, id, queue.StatusInProgress); err != nil {
				t.Fatal(err)
			}
			e.call(t, id, "ls")
			e.call(t, id, "make")
			if err := end(id); err != nil {
				t.Fatal(err)
			}
			rep := waitAnchors(t, e.s, o, 1)
			if rep.Records != 3 || rep.Ends != 1 || !rep.OK() {
				t.Fatalf("after the task ended: %+v", rep)
			}
			if c := copiesOf(t, e.s, o); len(c) != 1 || c[0].Seq != 3 {
				t.Fatalf("orch.db copies = %+v; want the end record's", c)
			}
		})
	}

	// A retry runs the failed task again: its records follow the end record
	// in the same chain, nothing is anchored while it runs, and its next end
	// is anchored again.
	id := "task-failed"
	o := toollog.Owner{Kind: toollog.OwnerTask, ID: id}
	if err := e.tasks.SetStatus(ctx, id, queue.StatusPending); err != nil {
		t.Fatal(err)
	}
	e.running.Store(id, true)
	if err := e.tasks.SetStatus(ctx, id, queue.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	e.call(t, id, "make test")
	// A change while the run is live anchors nothing.
	e.s.endTaskChain(ctx, id)
	e.running.Delete(id)
	if err := e.tasks.SaveResult(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}, workflows.Outcome{Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	rep := waitAnchors(t, e.s, o, 2)
	recs := records(t, e.s, o)
	if rep.Records != 5 || rep.Ends != 2 || !rep.OK() || recs[3].Command != "make test" || recs[4].Kind != toollog.KindEnd {
		t.Fatalf("after the retry ended: %+v, records %+v", rep, recs)
	}
	if c := copiesOf(t, e.s, o); len(c) != 2 || c[1].Seq != 5 {
		t.Fatalf("orch.db copies after the retry = %+v", c)
	}
	vr, err := e.s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "task:" + id})
	if err != nil || len(vr.GetProblems()) != 0 || vr.GetAnchors() != 2 {
		t.Fatalf("VerifyToolLog after the retry: %v, %v", vr, err)
	}
}

// TestTaskChainAnchorCatchUp: a task that ended while nothing followed the
// ledger (orchd down, or the watch fell behind) is anchored by the next
// retention sweep; one still running, pending or unknown is not.
func TestTaskChainAnchorCatchUp(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	for id, status := range map[string]string{"t-done": "complete", "t-live": "complete", "t-wait": queue.StatusPending, "t-gone": ""} {
		if status != "" {
			if err := e.tasks.Create(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}); err != nil {
				t.Fatal(err)
			}
			if err := e.tasks.SaveResult(ctx, workflows.AgentTaskInput{ID: id, Agent: "dev"}, workflows.Outcome{Status: status}); err != nil {
				t.Fatal(err)
			}
		}
		e.call(t, id, "ls")
	}
	e.running.Store("t-live", true)
	e.s.sweepToolLog(ctx, time.Now())
	waitAnchors(t, e.s, toollog.Owner{Kind: toollog.OwnerTask, ID: "t-done"}, 1)
	time.Sleep(50 * time.Millisecond)
	flushToolLog(t, e.s)
	for _, id := range []string{"t-live", "t-wait", "t-gone"} {
		if rep, _ := e.s.toolLog.store.Verify(ctx, toollog.Owner{Kind: toollog.OwnerTask, ID: id}); rep.Anchors != 0 {
			t.Errorf("%s was anchored: %+v", id, rep)
		}
	}
	// With no ledger reader nothing is anchored, and the anchorer does
	// not start.
	e.s.toolLog.taskStatus = nil
	e.s.endSettledTasks(ctx)
	e.s.runTaskAnchors(ctx)
}
