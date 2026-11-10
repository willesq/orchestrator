package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/cli"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// liveClock is a settable clock for liveness tests.
type liveClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *liveClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *liveClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// liveEnv is a pins server whose fake harnesses, claude 400 and codex 410,
// started shortly before base, the clock's starting time. base is the real
// time the test began, so command log events (stamped with the real clock)
// fall right after it.
type liveEnv struct {
	s     *server
	dirs  [2]string
	tbl   procTable
	clock *liveClock
	base  time.Time
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	s, dirs := pinsServer(t)
	base := time.Now()
	e := &liveEnv{s: s, dirs: dirs, clock: &liveClock{t: base}, base: base}
	e.tbl = procTable{
		started: map[int]time.Time{400: base.Add(-time.Minute), 410: base.Add(-30 * time.Second)},
		cwd:     map[int]string{400: dirs[0], 410: dirs[1]},
	}
	s.pins.started, s.pins.cwd, s.pins.now = e.tbl.startTime, e.tbl.workdir, e.clock.now
	s.pins.procs = func() ([]procstat.Process, error) { return nil, nil }
	return e
}

// hook is a hook process inside harness process pid of the fake table,
// nested in the outer harnesses given.
func (e *liveEnv) hook(harness string, pid int, outer ...procstat.Identity) context.Context {
	return hookIn(harness, procstat.Identity{PID: pid, Started: e.tbl.started[pid]}, outer...)
}

func hookIn(harness string, proc procstat.Identity, outer ...procstat.Identity) context.Context {
	return callerCtx(callerAuth{pid: 9000 + proc.PID, harness: harness, harnessProc: proc, outerHarnesses: outer})
}

func (e *liveEnv) pin(t *testing.T, ctx context.Context, id, cwd string) *orchdv1.SessionPin {
	t.Helper()
	p, err := e.s.PinSession(ctx, pinReq(id, cwd, ""))
	if err != nil {
		t.Fatalf("PinSession %s: %v", id, err)
	}
	return p
}

// session is a call from inside session id, pinned to harness process pid,
// as caller resolution would report it.
func (e *liveEnv) session(harness string, pid int, id string) context.Context {
	return sessionCtx(e.hook(harness, pid), sessionPin{SessionID: id, Harness: harness,
		Process: procstat.Identity{PID: pid, Started: e.tbl.started[pid]}})
}

func sessionCtx(ctx context.Context, pin sessionPin) context.Context {
	return context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerSession, pin: pin})
}

func listSessions(t *testing.T, s *server, ctx context.Context, all bool) map[string]*orchdv1.SessionInfo {
	t.Helper()
	rep, err := s.ListSessions(ctx, &orchdv1.ListSessionsRequest{All: all})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*orchdv1.SessionInfo{}
	for _, r := range rep.GetSessions() {
		k := r.GetSessionId()
		if k == "" {
			k = "pid:" + strconv.FormatInt(r.GetHarnessPid(), 10)
		}
		out[k] = r
	}
	return out
}

// TestSessionLivenessStates is SP-017.1.a1: a running session with a recent
// hook event is active; one with no event for 30 minutes is idle, and so is
// one left open for 7 days.
func TestSessionLivenessStates(t *testing.T) {
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	e.pin(t, e.hook("claude", 400), "sess-busy", e.dirs[0])
	e.pin(t, e.hook("codex", 410), "sess-quiet", e.dirs[1])
	if _, err := e.s.RecordToolEvent(e.session("claude", 400, "sess-busy"), bashReq("sess-busy", e.dirs[0], "ls")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)

	cases := []struct {
		name    string
		after   time.Duration
		session string
		want    string
	}{
		{"alive with a recent event", 29 * time.Minute, "sess-busy", stateActive},
		{"alive with no event for 30 min", 30 * time.Minute, "sess-quiet", stateIdle},
		{"alive with no event for 7 days", 7 * 24 * time.Hour, "sess-busy", stateIdle},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e.clock.set(e.base.Add(tt.after))
			row := listSessions(t, e.s, operatorCtx, false)[tt.session]
			if row == nil || row.GetState() != tt.want || row.GetEndedAt() != 0 {
				t.Fatalf("%s after %v = %v, want %s", tt.session, tt.after, row, tt.want)
			}
		})
	}
	row := listSessions(t, e.s, operatorCtx, false)["sess-busy"]
	if row.GetToolEvents() != 1 || row.GetRefusals() != 0 || row.GetLastActivity() < e.base.Unix() {
		t.Fatalf("sess-busy activity = %v", row)
	}
}

// TestProcessExitEndsSession is SP-017.1.a2: a harness that exits without
// SessionEnd is marked ended by the next sweep, with ended_at the time orchd
// noticed, and its command log chain is closed as SessionEnd would.
func TestProcessExitEndsSession(t *testing.T) {
	if sessionSweepEvery > time.Minute {
		t.Fatalf("sessions are swept every %v, want at most a minute", sessionSweepEvery)
	}
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	e.pin(t, e.hook("claude", 400), "sess-exit", e.dirs[0])
	e.pin(t, e.hook("codex", 410), "sess-stays", e.dirs[1])
	if _, err := e.s.RecordToolEvent(e.session("claude", 400, "sess-exit"), bashReq("sess-exit", e.dirs[0], "ls")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)

	delete(e.tbl.started, 400) // claude 400 exits; no SessionEnd comes
	noticed := e.base.Add(45 * time.Second)
	e.clock.set(noticed)
	e.s.sweepSessions(context.Background())

	p, ok, err := e.s.pins.get(context.Background(), "sess-exit")
	if err != nil || !ok || !p.EndedAt.Equal(noticed) {
		t.Fatalf("pin after the sweep = %+v, %v, %v; want ended at %v", p, ok, err, noticed)
	}
	if p, _, _ := e.s.pins.get(context.Background(), "sess-stays"); !p.EndedAt.IsZero() {
		t.Fatalf("a running session was ended: %+v", p)
	}
	rows := listSessions(t, e.s, operatorCtx, false)
	if _, listed := rows["sess-exit"]; listed || rows["sess-stays"] == nil {
		t.Fatalf("default list = %v, want only sess-stays", rows)
	}
	if r := listSessions(t, e.s, operatorCtx, true)["sess-exit"]; r.GetState() != stateEnded || r.GetEndedAt() != noticed.Unix() {
		t.Fatalf("--all row = %v, want ended at %d", r, noticed.Unix())
	}
	// A later sweep keeps the first end.
	e.clock.set(noticed.Add(time.Hour))
	e.s.sweepSessions(context.Background())
	if p, _, _ := e.s.pins.get(context.Background(), "sess-exit"); !p.EndedAt.Equal(noticed) {
		t.Fatalf("ended_at moved to %v", p.EndedAt)
	}
	waitForEndRecord(t, e.s, "sess-exit")
}

// waitForEndRecord waits for the end record the sweep's endToolLog writes in
// the background.
func waitForEndRecord(t *testing.T, s *server, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		recs := records(t, s, toollog.Owner{Kind: toollog.OwnerSession, ID: id})
		if n := len(recs); n > 0 && recs[n-1].Kind == toollog.KindEnd {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no end record for %s: %+v", id, recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSessionSweepLoop: orchd sweeps once at start and then on every tick
// until it stops.
func TestSessionSweepLoop(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-loop", e.dirs[0])
	var reads atomic.Int64
	started := e.s.pins.started
	e.s.pins.started = func(pid int) (time.Time, error) {
		reads.Add(1)
		return started(pid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.s.runSessionSweeps(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for reads.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d sweeps in 10 s", reads.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep loop did not stop")
	}
}

// TestReusedPIDEndsSession is SP-017.1.a3: when the kernel gives a pinned
// harness's PID to a new process (another start time), the old session is
// ended and is never tied to the new process.
func TestReusedPIDEndsSession(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-old", e.dirs[0])
	old := procstat.Identity{PID: 400, Started: e.tbl.started[400]}

	reused := e.base.Add(10 * time.Second)
	e.tbl.started[400] = reused
	noticed := e.base.Add(20 * time.Second)
	e.clock.set(noticed)
	e.s.sweepSessions(context.Background())

	ctx := context.Background()
	p, _, err := e.s.pins.get(ctx, "sess-old")
	if err != nil || !p.EndedAt.Equal(noticed) || !p.Process.Same(old) {
		t.Fatalf("old pin = %+v, %v; want ended at %v, still naming %v", p, err, noticed, old)
	}
	newProc := procstat.Identity{PID: 400, Started: reused}
	if p, ok, err := e.s.pins.currentOf(ctx, newProc); err != nil || ok {
		t.Fatalf("the new process resolves to pin %+v (%v)", p, err)
	}
	if p, ok, err := e.s.pins.firstOf(ctx, newProc); err != nil || ok {
		t.Fatalf("the new process has pin %+v (%v)", p, err)
	}
	newHook := hookIn("claude", newProc)
	if _, err := e.s.PinSession(newHook, pinReq("sess-old", e.dirs[0], "")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("PinSession from the new process = %v, want PermissionDenied", err)
	}
	if _, err := e.s.EndSession(newHook, &orchdv1.EndSessionRequest{SessionId: "sess-old"}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("EndSession from the new process = %v, want PermissionDenied", err)
	}
	// The new process shows as an unpinned claude, never as sess-old.
	e.s.pins.procs = func() ([]procstat.Process, error) {
		return []procstat.Process{{Identity: newProc, Name: "claude"}}, nil
	}
	rows := listSessions(t, e.s, operatorCtx, true)
	if r := rows["sess-old"]; r.GetState() != stateEnded {
		t.Fatalf("sess-old = %v, want ended", r)
	}
	if r := rows["pid:400"]; r.GetState() != stateUnpinned || r.GetSessionId() != "" {
		t.Fatalf("pid 400 = %v, want unpinned", r)
	}
}

// TestResumeRebindsPin is SP-017.1.a4: SessionStart with source=resume in a
// new harness process moves the pin to that process and start time, keeps
// its space, first directory and revision, and makes the session active.
func TestResumeRebindsPin(t *testing.T) {
	s, code, _ := placementServer(t)
	app := code + "/company1/app"
	tbl := withPins(t, s, app, code+"/willesq")
	s.pins.procs = func() ([]procstat.Process, error) { return nil, nil }
	orig, err := s.PinSession(inClaude, pinReq("sess-r", app, ""))
	if err != nil || orig.GetSpace() != "acme" {
		t.Fatalf("pin = %v, %v", orig, err)
	}
	// claude 400 exits without SessionEnd and is swept.
	delete(tbl.started, 400)
	s.sweepSessions(context.Background())
	if r := listSessions(t, s, operatorCtx, true)["sess-r"]; r.GetState() != stateEnded {
		t.Fatalf("before the resume = %v, want ended", r)
	}
	if p, _, _ := s.pins.get(context.Background(), "sess-r"); !p.ExitedAt.Equal(p.EndedAt) || p.ExitedAt.IsZero() {
		t.Fatalf("swept pin = %+v, want exited_at = ended_at (SP-015)", p)
	}
	// `claude --resume` starts claude 420 in the same directory.
	now := s.pins.now()
	resumed := now.Add(-10 * time.Second)
	tbl.started[420], tbl.cwd[420] = resumed, app
	ctx := hookIn("claude", procstat.Identity{PID: 420, Started: resumed})
	req := pinReq("sess-r", app, "")
	req.Source = "resume"
	pin, err := s.PinSession(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if pin.GetHarnessPid() != 420 || pin.GetHarnessStarted() != resumed.UnixNano() || pin.GetEndedAt() != 0 ||
		pin.GetSpace() != orig.GetSpace() || pin.GetFirstCwd() != orig.GetFirstCwd() ||
		pin.GetPolicyRevision() != orig.GetPolicyRevision() || pin.GetPinnedAt() != orig.GetPinnedAt() {
		t.Fatalf("resumed pin = %v, original %v", pin, orig)
	}
	stored, _, err := s.pins.get(context.Background(), "sess-r")
	if err != nil || !stored.ResumedAt.Equal(now) || !stored.EndedAt.IsZero() || !stored.ExitedAt.IsZero() {
		t.Fatalf("stored pin = %+v, %v", stored, err)
	}
	r := listSessions(t, s, operatorCtx, false)["sess-r"]
	if r.GetState() != stateActive || r.GetHarnessPid() != 420 || r.GetSpace() != "acme" || r.GetLastActivity() != now.Unix() {
		t.Fatalf("listed after the resume = %v", r)
	}
	// The new process's later calls resolve to the session.
	if p, ok, err := s.pins.currentOf(context.Background(), procstat.Identity{PID: 420, Started: resumed}); err != nil || !ok || p.SessionID != "sess-r" {
		t.Fatalf("currentOf(420) = %+v, %v, %v", p, ok, err)
	}
	// A connection of the old process that cached the pin no longer sees
	// the moved session's row.
	oldProc := procstat.Identity{PID: 400, Started: claudeStart}
	stale := sessionCtx(hookIn("claude", oldProc), sessionPin{SessionID: "sess-r", Process: oldProc})
	if rows := listSessions(t, s, stale, true); len(rows) != 0 {
		t.Fatalf("the old process's cached caller sees %v", rows)
	}
	// A repeated resume SessionStart changes nothing.
	again, err := s.PinSession(ctx, req)
	if err != nil || again.String() != pin.String() {
		t.Fatalf("repeated resume = %v, %v; want %v", again, err, pin)
	}
}

// TestResumeRefusals: a resume never takes a session from a process still
// running it, crosses harness programs, or moves it to a process that would
// get another space; in its own process it only clears ended_at.
func TestResumeRefusals(t *testing.T) {
	s, code, _ := placementServer(t)
	app, personal := code+"/company1/app", code+"/willesq"
	tbl := withPins(t, s, app, personal)
	if _, err := s.PinSession(inClaude, pinReq("sess-r", app, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinSession(inCodex, pinReq("sess-codex", personal, "")); err != nil {
		t.Fatal(err)
	}
	now := s.pins.now()
	newClaude := func(pid int, cwd string, outer ...procstat.Identity) context.Context {
		started := now.Add(-time.Duration(pid) * time.Millisecond)
		tbl.started[pid], tbl.cwd[pid] = started, cwd
		return hookIn("claude", procstat.Identity{PID: pid, Started: started}, outer...)
	}
	resume := func(ctx context.Context, id, cwd string) error {
		req := pinReq(id, cwd, "")
		req.Source = "resume"
		_, err := s.PinSession(ctx, req)
		return err
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		p, _, err := s.pins.get(context.Background(), "sess-r")
		if err != nil || p.Process.PID != 400 || p.Space != "acme" || !p.ResumedAt.IsZero() {
			t.Fatalf("pin changed: %+v, %v", p, err)
		}
	}

	if err := resume(newClaude(420, app), "sess-r", app); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("resume while claude 400 runs = %v, want PermissionDenied", err)
	}
	unchanged(t)

	delete(tbl.started, 400)
	s.sweepSessions(context.Background())
	if err := resume(inCodex, "sess-r", app); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("resume in codex = %v, want PermissionDenied", err)
	}
	if err := resume(newClaude(430, personal), "sess-r", personal); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resume where the space is personal = %v, want FailedPrecondition", err)
	}
	codex := procstat.Identity{PID: 410, Started: tbl.started[410]}
	if err := resume(newClaude(440, app, codex), "sess-r", app); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resume inside the personal codex session = %v, want FailedPrecondition", err)
	}
	unchanged(t)

	// A harness that already holds a pin in the same space (an agent calling
	// PinSession itself, or an in-process /resume) cannot take the dead
	// session's pin, nor can a harness nested in a same-space session.
	same := newClaude(450, app)
	if _, err := s.PinSession(same, pinReq("sess-agent", app, "")); err != nil {
		t.Fatal(err)
	}
	if err := resume(same, "sess-r", app); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resume from a pinned acme harness = %v, want FailedPrecondition", err)
	}
	outer := procstat.Identity{PID: 450, Started: tbl.started[450]}
	if err := resume(newClaude(460, app, outer), "sess-r", app); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resume nested in an acme session = %v, want FailedPrecondition", err)
	}
	unchanged(t)
	// Its events then stay in its own chain: sess-r is not on its process.
	if p, _, _ := s.pins.currentOf(context.Background(), outer); p.SessionID != "sess-agent" {
		t.Fatalf("harness 450 resolves to %q", p.SessionID)
	}

	// In its own process, after SessionEnd, a resume clears ended_at.
	if _, err := s.EndSession(inCodex, &orchdv1.EndSessionRequest{SessionId: "sess-codex"}); err != nil {
		t.Fatal(err)
	}
	req := pinReq("sess-codex", personal, "")
	req.Source = "resume"
	p, err := s.PinSession(inCodex, req)
	if err != nil || p.GetEndedAt() != 0 || p.GetHarnessPid() != 410 || p.GetSpace() != "personal" {
		t.Fatalf("resume in its own process = %v, %v", p, err)
	}
}

// TestSessionsListCommand is SP-017.2.a1: `orch sessions list` against
// orchd's gRPC server shows active and idle sessions with the nine columns,
// and ended ones only with --all.
func TestSessionsListCommand(t *testing.T) {
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	e.pin(t, e.hook("codex", 410), "sess-idle", e.dirs[1])
	e.pin(t, e.hook("claude", 400), "sess-ended", e.dirs[0])
	if _, err := e.s.EndSession(e.hook("claude", 400), &orchdv1.EndSessionRequest{SessionId: "sess-ended"}); err != nil {
		t.Fatal(err)
	}
	later := e.base.Add(31 * time.Minute)
	e.clock.set(later)
	e.tbl.started[420], e.tbl.cwd[420] = later.Add(-time.Minute), e.dirs[0]
	e.pin(t, e.hook("claude", 420), "sess-active", e.dirs[0])

	run := listCommand(t, e.s)
	out := run("sessions", "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	header := strings.Join(strings.Fields(lines[0]), " ")
	if header != "HARNESS SESSION PID SPACE DIRECTORY STATE LAST ACTIVITY EVENTS REFUSALS" {
		t.Fatalf("header = %q", lines[0])
	}
	want := map[string][]string{
		"sess-active": {"claude", "sess-active", "420", "-", e.dirs[0], stateActive, "0", "0"},
		"sess-idle":   {"codex", "sess-idle", "410", "-", e.dirs[1], stateIdle, "0", "0"},
	}
	checkRows(t, lines[1:], want)

	out = run("sessions", "list", "--all")
	want["sess-ended"] = []string{"claude", "sess-ended", "400", "-", e.dirs[0], stateEnded, "0", "0"}
	checkRows(t, strings.Split(strings.TrimSpace(out), "\n")[1:], want)
}

// checkRows matches each table row to want by its session column. A row has
// nine columns; LAST ACTIVITY is a date and a time, so ten fields.
func checkRows(t *testing.T, rows []string, want map[string][]string) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %d", rows, len(want))
	}
	for _, line := range rows {
		f := strings.Fields(line)
		if len(f) != 10 {
			t.Fatalf("row %q has %d fields, want 10", line, len(f))
		}
		w, ok := want[f[1]]
		got := append(f[:6:6], f[8], f[9])
		if !ok || strings.Join(got, " ") != strings.Join(w, " ") {
			t.Fatalf("row %q, want %v", line, w)
		}
		if _, err := time.ParseInLocation("2006-01-02 15:04", f[6]+" "+f[7], time.Local); err != nil {
			t.Fatalf("row %q: last activity: %v", line, err)
		}
	}
}

// listCommand serves s over a socket as the operator and returns a function
// that runs the real CLI against it.
func listCommand(t *testing.T, s *server) func(args ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "orch-sessions-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Reason: this test may itself run under an agent harness, which orchd
	// would show only its own row; the CLI here stands for the operator.
	asOperator := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(peer.NewContext(ctx, &peer.Peer{AuthInfo: callerAuth{pid: 42}}), req)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(asOperator))
	orchdv1.RegisterOrchdServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	t.Setenv("ORCH_SOCKET", sock)
	return func(args ...string) string {
		t.Helper()
		root := cli.NewRootCmd(orchdclient.Dial)
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("orch %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
}

// TestUnpinnedHarnessesListed is SP-017.2.a2: running harness processes with
// no live pin are listed as unpinned, found by kernel name as
// procstat.FindByName does, or by program for an interpreter or a binary
// named for its version.
func TestUnpinnedHarnessesListed(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-pinned", e.dirs[0])
	e.pin(t, e.hook("codex", 410), "sess-gone", e.dirs[1])
	if _, err := e.s.EndSession(e.hook("codex", 410), &orchdv1.EndSessionRequest{SessionId: "sess-gone"}); err != nil {
		t.Fatal(err)
	}
	at := e.base.Add(-time.Hour)
	proc := func(pid int, name string) procstat.Process {
		return procstat.Process{Identity: procstat.Identity{PID: pid, Started: at}, Name: name}
	}
	table := []procstat.Process{
		{Identity: procstat.Identity{PID: 400, Started: e.tbl.started[400]}, Name: "claude"}, // pinned
		{Identity: procstat.Identity{PID: 410, Started: e.tbl.started[410]}, Name: "codex"},  // its only pin ended
		proc(501, "claude"), proc(502, "codex"), proc(503, "gemini"),
		proc(504, "node"),   // Claude Code from npm
		proc(505, "2.1.31"), // Claude Code's native binary
		proc(506, "node"),   // some other script
		proc(507, "zsh"),    // not a harness
		proc(508, "Claude"), // the desktop app
		proc(509, "node"),   // gemini-cli
		proc(510, "node"),   // program unreadable
		proc(511, "9.9.9"),  // a version-named binary that is no harness
		{Identity: procstat.Identity{PID: 400, Started: at}, Name: "claude"}, // a different process than the pinned 400
	}
	progs := map[int]procstat.Program{
		504: {Name: "node", Args: []string{"node", "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js"}},
		505: {Name: "2.1.31", Path: "/Users/u/.local/share/claude/versions/2.1.31"},
		506: {Name: "node", Args: []string{"node", "/srv/app/server.js"}},
		509: {Name: "node", Args: []string{"node", "/opt/homebrew/lib/node_modules/@google/gemini-cli/dist/index.js"}},
		511: {Name: "9.9.9", Path: "/opt/tool/9.9.9"},
	}
	e.s.pins.procs = func() ([]procstat.Process, error) { return table, nil }
	e.s.pins.program = func(pid int) (procstat.Program, error) {
		if p, ok := progs[pid]; ok {
			return p, nil
		}
		return procstat.Program{}, errors.New("no such process")
	}
	rep, err := e.s.ListSessions(operatorCtx, &orchdv1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rep.GetSessions() {
		if r.GetState() == stateUnpinned {
			if r.GetSessionId() != "" || r.GetSpace() != "" || r.GetFirstCwd() != "" {
				t.Errorf("unpinned row carries session fields: %v", r)
			}
			got = append(got, strconv.FormatInt(r.GetHarnessPid(), 10)+":"+r.GetHarness())
		}
	}
	sort.Strings(got)
	want := "400:claude 410:codex 501:claude 502:codex 503:gemini 504:claude 505:claude 509:gemini"
	if strings.Join(got, " ") != want {
		t.Fatalf("unpinned = %v, want %s", got, want)
	}
	// Where the process table cannot be read, nothing is unpinned.
	e.s.pins.procs = func() ([]procstat.Process, error) { return nil, procstat.ErrUnsupported }
	if rows := listSessions(t, e.s, operatorCtx, false); len(rows) != 1 || rows["sess-pinned"] == nil {
		t.Fatalf("rows without a process table = %v", rows)
	}
}

// TestListSessionsCallerScope is SP-017.2.a3: agent callers see only their
// own session's row; the operator sees every session and unpinned process.
func TestListSessionsCallerScope(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-a", e.dirs[0])
	e.pin(t, e.hook("codex", 410), "sess-b", e.dirs[1])
	e.s.pins.procs = func() ([]procstat.Process, error) {
		return []procstat.Process{{Identity: procstat.Identity{PID: 600, Started: e.base}, Name: "claude"}}, nil
	}
	if rows := listSessions(t, e.s, operatorCtx, true); len(rows) != 3 || rows["sess-a"] == nil || rows["sess-b"] == nil || rows["pid:600"] == nil {
		t.Fatalf("operator sees %v", rows)
	}
	inA := e.session("claude", 400, "sess-a")
	for _, all := range []bool{false, true} {
		if rows := listSessions(t, e.s, inA, all); len(rows) != 1 || rows["sess-a"] == nil {
			t.Fatalf("session sess-a (all=%v) sees %v", all, rows)
		}
	}
	task := context.WithValue(e.hook("claude", 400), resolvedKey{}, resolvedCaller{kind: callerTask})
	unpinned := hookIn("claude", procstat.Identity{PID: 600, Started: e.base})
	for name, ctx := range map[string]context.Context{"queued task": task, "unpinned harness": unpinned} {
		if rows := listSessions(t, e.s, ctx, true); len(rows) != 0 {
			t.Fatalf("%s sees %v", name, rows)
		}
	}
	// A session whose pin ended sees its row only with --all.
	if _, err := e.s.EndSession(e.hook("claude", 400), &orchdv1.EndSessionRequest{SessionId: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	if rows := listSessions(t, e.s, inA, false); len(rows) != 0 {
		t.Fatalf("ended sess-a sees %v by default", rows)
	}
	if rows := listSessions(t, e.s, inA, true); len(rows) != 1 || rows["sess-a"].GetState() != stateEnded {
		t.Fatalf("ended sess-a sees %v with --all", rows)
	}
	if _, err := (&server{}).ListSessions(operatorCtx, &orchdv1.ListSessionsRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("ListSessions without pins = %v, want Unavailable", err)
	}
}

// TestSessionSweepCost is SP-017.2.a4: one liveness sweep over 50 pinned
// sessions, each a distinct real process read from the kernel, takes under
// 50 ms. The first sweep also ends 25 pins whose start time does not match.
func TestSessionSweepCost(t *testing.T) {
	s, dirs := pinsServer(t)
	s.pins.started = procstat.StartTime
	procs := realProcesses(t, 50)
	ctx := context.Background()
	for i, p := range procs {
		id := p
		if i%2 == 1 {
			id.Started = id.Started.Add(-time.Hour) // a reused PID: ended by the sweep
		}
		pin := sessionPin{SessionID: "cost-" + strconv.Itoa(i), Harness: "claude", Process: id,
			FirstCwd: dirs[0], PinnedAt: time.Now().UTC()}
		if err := s.pins.insert(ctx, pin); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	gone, err := s.pins.sweep(ctx)
	first := time.Since(start)
	if err != nil || len(gone) < 25 {
		t.Fatalf("first sweep ended %d pins (%v), want at least 25", len(gone), err)
	}
	// The other 25 now hold live pins: resweep them every time.
	if _, err := s.pins.db.ExecContext(ctx, `UPDATE session_pins SET ended_at = NULL`); err != nil {
		t.Fatal(err)
	}
	for i, p := range procs {
		if _, err := s.pins.db.ExecContext(ctx, `UPDATE session_pins SET harness_started = ? WHERE session_id = ?`,
			p.Started.UnixNano(), "cost-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	var times []time.Duration
	for range 20 {
		start := time.Now()
		if _, err := s.pins.sweep(ctx); err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("sweep of 50 pins: first (ending %d) %v; steady median %v, max %v", len(gone), first, times[len(times)/2], times[len(times)-1])
	if raceEnabled {
		return
	}
	if worst := max(first, times[len(times)-1]); worst >= 50*time.Millisecond {
		t.Fatalf("a sweep took %v, want < 50 ms", worst)
	}
}

// realProcesses returns n distinct running processes with their kernel
// start times: from the process table on macOS, else from /proc.
func realProcesses(t *testing.T, n int) []procstat.Identity {
	t.Helper()
	var pids []int
	if procs, err := procstat.Processes(); err == nil {
		for _, p := range procs {
			pids = append(pids, p.PID)
		}
	} else if entries, err := os.ReadDir("/proc"); err == nil {
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	var out []procstat.Identity
	for _, pid := range pids {
		started, err := procstat.StartTime(pid)
		if err != nil {
			continue
		}
		out = append(out, procstat.Identity{PID: pid, Started: started})
		if len(out) == n {
			return out
		}
	}
	t.Skipf("only %d readable processes, want %d", len(out), n)
	return nil
}
