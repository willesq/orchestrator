package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
	"github.com/willesq/orchestrator/internal/workflows"
)

// Spaces SP-029 (INV-09): derived content carries the space of what it came
// from.

// callSummarizer records the messages of every call.
type callSummarizer struct {
	mu    sync.Mutex
	calls [][]session.Message
}

func (c *callSummarizer) Summarize(_ context.Context, msgs []session.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, slices.Clone(msgs))
	return "summary of " + msgs[0].Text, nil
}

// harnessTranscript writes session id's transcript where Claude Code keeps
// it under claude, with one user turn saying text.
func harnessTranscript(t *testing.T, claude, id, text string) string {
	t.Helper()
	dir := filepath.Join(claude, "-proj-"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	body := `{"type":"user","message":{"role":"user","content":"` + text + `"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"working on ` + text + `"}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// moveTranscriptRoots points transcriptRoots at a temporary Claude Code
// projects folder, which it returns.
func moveTranscriptRoots(t *testing.T) string {
	t.Helper()
	claude, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prev := transcriptRoots
	transcriptRoots = func() (string, string) { return claude, "" }
	t.Cleanup(func() { transcriptRoots = prev })
	return claude
}

// pinAt records a pin of session id in space, as PinSession would.
func pinAt(t *testing.T, s *server, id, space string, pid int) {
	t.Helper()
	p := sessionPin{SessionID: id, Harness: "claude", Process: procstat.Identity{PID: pid, Started: claudeStart},
		FirstCwd: "/", Space: space, Revision: 1, PinnedAt: time.Now().UTC()}
	if err := s.pins.insert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// TestSummaryHasItsSessionsSpace is SP-029.1.a1: every summarizer call
// gets the messages of one session only, and its summary is placed in that
// session's space. Two sessions in two isolated spaces are queued together;
// a summary asked for by another session is quarantined (SP-023).
func TestSummaryHasItsSessionsSpace(t *testing.T) {
	e := newReadEnv(t)
	claude := moveTranscriptRoots(t)
	pinAt(t, e.s, "sess-a", "acme", 501)
	pinAt(t, e.s, "sess-g", "globex", 502)
	jobs := []summaryJob{
		{SessionID: "sess-a", TranscriptPath: harnessTranscript(t, claude, "sess-a", "acme-only-marker")},
		{SessionID: "sess-g", TranscriptPath: harnessTranscript(t, claude, "sess-g", "globex-only-marker")},
	}
	want := map[string]string{"sess-a": "acme", "sess-g": "globex"}
	marker := map[string]string{"sess-a": "acme-only-marker", "sess-g": "globex-only-marker"}

	sum := &callSummarizer{}
	ing := newRecordingIngester()
	q := newSummaryQueue()
	for _, j := range jobs {
		if !q.enqueue(j) {
			t.Fatal("queue full")
		}
	}
	// Drained by the unit, as orchd does, so both jobs are waiting at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := summariesUnit(q, sum, ing, time.Second)
	if err := u.Run(ctx, func() {}); err != nil {
		t.Fatal(err)
	}
	placer := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
	seen := map[string]bool{}
	for range jobs {
		var m rag.Meta
		select {
		case m = <-ing.metas:
			<-ing.texts
		case <-time.After(10 * time.Second):
			t.Fatal("no summary stored")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(m.SourceURI, "session://"), "/record")
		if m.Writer != rag.WriterSession || m.Origin.Session != id {
			t.Fatalf("summary of %s stored with %+v", id, m)
		}
		if err := placer.place(context.Background(), &m); err != nil || m.Space != want[id] {
			t.Fatalf("summary of %s placed in %q, %v; want %s", id, m.Space, err, want[id])
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("summaries stored for %v", seen)
	}
	sum.mu.Lock()
	calls := slices.Clone(sum.calls)
	sum.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("summarizer called %d times, want once per session", len(calls))
	}
	for _, msgs := range calls {
		var mine string
		for id, mk := range marker {
			if strings.Contains(msgs[0].Text, mk) {
				mine = id
			}
		}
		for _, msg := range msgs {
			for id, mk := range marker {
				if id != mine && strings.Contains(msg.Text, mk) {
					t.Fatalf("one summarizer call holds text of %s and %s: %+v", mine, id, msgs)
				}
			}
		}
		if mine == "" {
			t.Fatalf("a summarizer call holds no session's text: %+v", msgs)
		}
	}

	// A summary another session asked for is tied to no session.
	summarizeOne(context.Background(), sum, ing, summaryJob{SessionID: "sess-a", TranscriptPath: jobs[0].TranscriptPath, Foreign: true}, time.Second)
	m := <-ing.metas
	<-ing.texts
	if err := placer.place(context.Background(), &m); err != nil || m.Space != spaces.Quarantine {
		t.Fatalf("a foreign summary placed in %q, %v", m.Space, err)
	}
}

// taskEnv is a readEnv with a task the worker runs: dispatched from a
// session in acme, with its directory in globex's folder.
type derivedTaskEnv struct {
	readEnv
	runs   map[string]agentrun.LiveRun
	guard  *guardWriter
	pinner *taskPinner
}

func newDerivedTaskEnv(t *testing.T) derivedTaskEnv {
	t.Helper()
	e := derivedTaskEnv{readEnv: newReadEnv(t), runs: map[string]agentrun.LiveRun{}}
	e.guard = newGuardWriter(t.TempDir(), "/home/u", "", "", "")
	e.pinner = &taskPinner{pins: e.s.pins, spaces: e.s.spaces, guard: e.guard,
		run: func(id string) (agentrun.LiveRun, bool) { r, ok := e.runs[id]; return r, ok }}
	return e
}

// TestTaskRunsPinnedToItsDispatchersSpace is SP-029.1.a2: a task dispatched
// from a session in acme is pinned to acme (its session's pin and the
// guard's pin file), searches as acme, and its events, session records and
// summary are in acme, although it runs in globex's folder.
func TestTaskRunsPinnedToItsDispatchersSpace(t *testing.T) {
	e := newDerivedTaskEnv(t)
	var recorded string
	e.s.disp = spaceDispatcher{got: &recorded}
	if _, err := e.s.Dispatch(inSpace("acme"), &orchdv1.DispatchRequest{Body: "x", Cwd: e.code + "/globex/repo"}); err != nil {
		t.Fatal(err)
	}
	if recorded != "acme" {
		t.Fatalf("dispatch recorded %q", recorded)
	}
	in := workflows.AgentTaskInput{ID: "task-1", Cwd: e.code + "/globex/repo", Space: recorded}
	run := agentTask(in)
	sid := agentrun.SessionID(run.ID)
	live := agentrun.LiveRun{TaskID: run.ID, SessionID: sid, Dir: run.Cwd, Space: run.Space}

	// Before its agent process starts there is nothing to pin.
	e.runs[run.ID] = live
	e.pinner.changed(run.ID)
	if _, ok, _ := e.s.pins.get(context.Background(), sid); ok {
		t.Fatal("pinned before the agent process started")
	}
	live.PID, live.ProcStarted = 600, claudeStart
	e.runs[run.ID] = live
	e.pinner.changed(run.ID)

	// Pinned to acme.
	pin, ok, err := e.s.pins.get(context.Background(), sid)
	if err != nil || !ok || pin.Space != "acme" || pin.Harness != "claude" || pin.Process.PID != 600 || pin.FirstCwd != run.Cwd {
		t.Fatalf("task pin = %+v, %v, %v", pin, ok, err)
	}
	gp, err := spaces.ReadGuardPin(e.guard.dir, sid)
	if err != nil || gp.Space != "acme" {
		t.Fatalf("guard pin file = %+v, %v", gp, err)
	}
	// The guard judges its calls in acme: its own folder is globex's, so
	// a call there is refused rather than refused for a missing pin.
	res, _, err := e.s.spaces.CurrentRevision()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := spaces.NewGuardPolicy(res, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := spaces.GuardEvent{Kind: spaces.EventPreToolUse, Tool: "Read", Cwd: run.Cwd,
		Targets: spaces.ToolTargets{Paths: []string{run.Cwd + "/main.go"}}}
	d := spaces.Decide(pol, spaces.GuardPin{Found: true, Space: gp.Space}, spaces.GuardEnforcement{Guard: spaces.GuardBlockOtherJobs}, ev)
	if d.Verdict != spaces.Refuse || d.Rule == spaces.RulePinMissing {
		t.Fatalf("guard decision in globex's folder = %+v", d)
	}
	ev.Cwd, ev.Targets.Paths = e.code+"/acme/repo", []string{e.code + "/acme/repo/main.go"}
	if d := spaces.Decide(pol, spaces.GuardPin{Found: true, Space: gp.Space}, spaces.GuardEnforcement{Guard: spaces.GuardBlockOtherJobs}, ev); d.Verdict != spaces.Allow {
		t.Fatalf("guard decision in acme's folder = %+v", d)
	}
	// The command log's replay judges the task's events in acme too.
	if rp, err := e.s.replayPin(context.Background(), toollog.Owner{Kind: toollog.OwnerTask, ID: run.ID}); err != nil || !rp.Found || rp.Space != "acme" {
		t.Fatalf("replay pin of the task = %+v, %v", rp, err)
	}

	// It searches as acme: acme's memory and personal, which acme lists,
	// never globex's.
	task := as(resolvedCaller{kind: callerTask, task: live})
	if got := e.readAll(t, task, memoText("acme", 3), memoURI("acme", 3)); !slices.Contains(got, memoURI("acme", 3)) {
		t.Error("the task cannot read acme")
	}
	if l := spaceLeaks([]string{"acme", "personal", "oss"}, e.readAll(t, task, memoText("globex", 3), memoURI("globex", 3))...); len(l) > 0 {
		t.Errorf("the task read %q", l[:min(3, len(l))])
	}

	// Its completion event is in acme.
	notifyTaskDone(in, workflows.Outcome{Status: "complete"})
	evs, err := events.Read(paths.EventsDir(), taskEventStream)
	if err != nil || len(evs) == 0 {
		t.Fatalf("queue events = %v, %v", evs, err)
	}
	if last := evs[len(evs)-1]; last.Space != "acme" || last.TaskID != run.ID {
		t.Fatalf("completion event = %+v", last)
	}

	// Its session's transcript, record and summary are placed in acme.
	placer := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
	for _, uri := range []string{"/transcript", "/record"} {
		m := rag.Meta{SourceURI: "session://" + sid + uri, Writer: rag.WriterSession,
			Origin: rag.Origin{System: rag.OriginSession, Session: sid, Path: run.Cwd}}
		if err := placer.place(context.Background(), &m); err != nil || m.Space != "acme" {
			t.Fatalf("the task's %s placed in %q, %v", uri, m.Space, err)
		}
	}

	// Pinning again as the run changes leaves the pin as it is.
	e.pinner.changed(run.ID)
	again, _, _ := e.s.pins.get(context.Background(), sid)
	if !again.PinnedAt.Equal(pin.PinnedAt) {
		t.Fatalf("pin rewritten: %+v then %+v", pin, again)
	}
}

// TestTaskPinReplacesASquatter: a session that pinned a task's session ID in
// another space before the task ran (through PinSession, which a harness
// may call with any ID) loses it to the task, so the task's records never
// reach that space.
func TestTaskPinReplacesASquatter(t *testing.T) {
	e := newDerivedTaskEnv(t)
	sid := agentrun.SessionID("task-2")
	pinAt(t, e.s, sid, "globex", 700)
	e.runs["task-2"] = agentrun.LiveRun{TaskID: "task-2", SessionID: sid, Dir: e.code + "/acme/repo", Space: "acme",
		PID: 601, ProcStarted: claudeStart}
	e.pinner.changed("task-2")
	pin, ok, err := e.s.pins.get(context.Background(), sid)
	if err != nil || !ok || pin.Space != "acme" || pin.Process.PID != 601 {
		t.Fatalf("pin after the task started = %+v, %v, %v", pin, ok, err)
	}
	// A squatter after the task started is refused: the pin names the
	// task's process.
	if _, err := e.s.pins.pin(context.Background(), sid, "", "", harnessCaller{name: "claude", proc: procstat.Identity{PID: 400, Started: claudeStart}},
		e.code+"/globex/repo", e.s.placeSession); err == nil {
		t.Fatal("another harness took the task's pin")
	}
	// Restarted in the same space by the same task (orchd restarted), it is
	// the task's pin again, now on the new process.
	r := e.runs["task-2"]
	r.PID = 602
	e.runs["task-2"] = r
	e.pinner.changed("task-2")
	if pin, _, _ := e.s.pins.get(context.Background(), sid); pin.Process.PID != 602 || pin.Space != "acme" {
		t.Fatalf("pin after a resume = %+v", pin)
	}
}

// TestTaskPinEdges: a task with no recorded space is pinned to quarantine;
// one whose session ID is not known yet (a fresh Codex run) or with no
// runner gets none; with no spaces policy nothing is pinned (INV-10).
func TestTaskPinEdges(t *testing.T) {
	e := newDerivedTaskEnv(t)
	for id, r := range map[string]agentrun.LiveRun{
		"no-space":    {SessionID: agentrun.SessionID("no-space"), PID: 610, ProcStarted: claudeStart},
		"gone-space":  {SessionID: agentrun.SessionID("gone-space"), Space: "dropped", PID: 611, ProcStarted: claudeStart},
		"codex-fresh": {Provider: agentrun.ProviderCodex, PID: 612, ProcStarted: claudeStart},
		"no-start":    {SessionID: agentrun.SessionID("no-start"), PID: 613},
	} {
		r.TaskID = id
		e.runs[id] = r
		e.pinner.changed(id)
	}
	for _, id := range []string{"no-space", "gone-space"} {
		if p, ok, _ := e.s.pins.get(context.Background(), agentrun.SessionID(id)); !ok || p.Space != spaces.Quarantine {
			t.Errorf("%s: pin = %+v, %v", id, p, ok)
		}
	}
	if p, ok, _ := e.s.pins.get(context.Background(), agentrun.SessionID("no-start")); ok {
		t.Errorf("a run with no process start was pinned: %+v", p)
	}
	// A resumed Codex run knows its thread.
	e.runs["codex-resumed"] = agentrun.LiveRun{TaskID: "codex-resumed", Provider: agentrun.ProviderCodex, SessionID: "thread-1",
		Space: "globex", PID: 614, ProcStarted: claudeStart}
	e.pinner.changed("codex-resumed")
	if p, ok, _ := e.s.pins.get(context.Background(), "thread-1"); !ok || p.Harness != "codex" || p.Space != "globex" {
		t.Errorf("codex pin = %+v, %v", p, ok)
	}
	e.pinner.changed("not-running")
	(*taskPinner)(nil).changed("x")

	// With no policy applied, nothing is pinned.
	s, _ := pinsServer(t)
	plain := &taskPinner{pins: s.pins, spaces: s.spaces, run: func(string) (agentrun.LiveRun, bool) {
		return agentrun.LiveRun{TaskID: "t", SessionID: agentrun.SessionID("t"), Space: "acme", PID: 620, ProcStarted: claudeStart}, true
	}}
	plain.changed("t")
	if p, ok, _ := s.pins.get(context.Background(), agentrun.SessionID("t")); ok {
		t.Fatalf("pinned with no policy: %+v", p)
	}
	// With no task's space recorded, the event carries none, as before.
	notifyTaskDone(workflows.AgentTaskInput{ID: "task-plain"}, workflows.Outcome{Status: "complete"})
	evs, _ := events.Read(paths.EventsDir(), taskEventStream)
	if last := evs[len(evs)-1]; last.Space != "" {
		t.Fatalf("event with no policy = %+v", last)
	}
}

// TestTaskPinFailures: under a policy that no longer verifies a task is
// pinned to quarantine (INV-03); a guard pin file that cannot be written
// leaves the pin; a store that fails pins nothing; and with no runner
// there is no run to pin.
func TestTaskPinFailures(t *testing.T) {
	e := newDerivedTaskEnv(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(`UPDATE spaces_revisions SET doc = doc || ' '`); err != nil {
		t.Fatal(err)
	}
	broken, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }, LookupOrg: orgNodes{}.lookup})
	if err != nil {
		t.Fatal(err)
	}
	// A guard directory under a regular file cannot be written.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.pinner.spaces, e.pinner.guard = broken, newGuardWriter(filepath.Join(file, "guard"), "/home/u", "", "", "")
	sid := agentrun.SessionID("task-b")
	e.runs["task-b"] = agentrun.LiveRun{TaskID: "task-b", SessionID: sid, Space: "acme", PID: 630, ProcStarted: claudeStart}
	e.pinner.changed("task-b")
	if p, ok, _ := e.s.pins.get(context.Background(), sid); !ok || p.Space != spaces.Quarantine || p.Revision != 0 {
		t.Fatalf("pin under a broken policy = %+v, %v", p, ok)
	}

	closed, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	failing := &taskPinner{pins: newPinRegistry(closed), spaces: e.s.spaces, run: e.pinner.run}
	failing.changed("task-b")

	if _, ok := (&liveRunner{}).run("task-b"); ok {
		t.Fatal("a run with no runner")
	}
	if _, ok := (&liveRunner{r: &agentrun.Runner{}}).run("task-b"); ok {
		t.Fatal("a run the runner is not running")
	}
}

// TestTaskSessionIDsArePinnedByOrchdOnly is the SP-029 security review's
// first finding: under a policy no harness may take a new pin under a
// queued task's session ID before the task runs (it would place the
// task's records in its own space); once orchd has pinned the task, the
// ID is pinned elsewhere for any other process. With no policy PinSession
// is as before (INV-10). Replay trusts only the pin orchd took for the task.
func TestTaskSessionIDsArePinnedByOrchdOnly(t *testing.T) {
	e := newDerivedTaskEnv(t)
	sid := agentrun.SessionID("task-s")
	if _, err := e.s.PinSession(inClaude, pinReq(sid, e.code+"/globex/repo", "")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("PinSession under a task's session ID = %v", err)
	}
	if _, ok, _ := e.s.pins.get(context.Background(), sid); ok {
		t.Fatal("the refused pin was recorded")
	}
	e.runs["task-s"] = agentrun.LiveRun{TaskID: "task-s", SessionID: sid, Dir: e.code + "/acme/repo", Space: "acme", PID: 640, ProcStarted: claudeStart}
	e.pinner.changed("task-s")
	if _, err := e.s.PinSession(inClaude, pinReq(sid, e.code+"/globex/repo", "")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("PinSession over the task's pin = %v", err)
	}
	if p, _, _ := e.s.pins.get(context.Background(), sid); p.Space != "acme" || p.InheritedFrom != "task:task-s" {
		t.Fatalf("task pin = %+v", p)
	}

	// Replay ignores a pin under a task's ID that orchd did not take for it.
	pinAt(t, e.s, agentrun.SessionID("task-other"), "globex", 650)
	if rp, err := e.s.replayPin(context.Background(), toollog.Owner{Kind: toollog.OwnerTask, ID: "task-other"}); err != nil || rp.Found {
		t.Fatalf("replay trusted a pin orchd did not take: %+v, %v", rp, err)
	}

	// Nor does placement or the guard: a session record under that ID is
	// quarantined, and the sync writes no pin file for it (and removes one).
	other := agentrun.SessionID("task-other")
	m := rag.Meta{SourceURI: "session://" + other + "/record", Writer: rag.WriterSession,
		Origin: rag.Origin{System: rag.OriginSession, Session: other}}
	if err := (&writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}).place(context.Background(), &m); err != nil || m.Space != spaces.Quarantine {
		t.Fatalf("record under a foreign pin on a task ID placed in %q, %v", m.Space, err)
	}
	if err := spaces.WriteGuardPin(e.guard.dir, spaces.GuardPinFile{SessionID: other, Space: "globex"}); err != nil {
		t.Fatal(err)
	}
	e.s.guard = e.guard
	e.s.syncGuardPins(context.Background())
	if _, err := spaces.ReadGuardPin(e.guard.dir, other); err == nil {
		t.Fatal("the sync kept a pin file for a foreign pin on a task ID")
	}
	if gp, err := spaces.ReadGuardPin(e.guard.dir, sid); err != nil || gp.Space != "acme" {
		t.Fatalf("the sync lost the task's pin file: %+v, %v", gp, err)
	}

	// A guard pin file that cannot be written is removed, so no file left
	// by an earlier pin judges the task's calls.
	sid2 := agentrun.SessionID("task-g")
	stale := spaces.GuardPinPath(e.guard.dir, sid2)
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	e.runs["task-g"] = agentrun.LiveRun{TaskID: "task-g", SessionID: sid2, Space: "acme", PID: 660, ProcStarted: claudeStart}
	e.pinner.changed("task-g")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("an unwritable pin file was left: %v", err)
	}

	// With no policy a harness pins such an ID as before.
	s, dirs := pinsServer(t)
	if _, err := s.PinSession(inClaude, pinReq(sid, dirs[0], "")); err != nil {
		t.Fatalf("PinSession with no policy = %v", err)
	}
}
