package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/peer"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// guardJobsPolicy binds two jobs under /home/u, spacesServer's home.
const guardJobsPolicy = `{"spaces":[{"name":"work","kind":"isolated","roots":["~/code/work"]},` +
	`{"name":"other","kind":"isolated","roots":["~/code/other"]}]}`

// withGuardFiles gives s a guard writer in a temporary directory and starts
// it, as Run does.
func withGuardFiles(t *testing.T, s *server) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "guard")
	s.guard = newGuardWriter(dir, "/home/u", "/home/u/.config/orch", filepath.Dir(dir), "/home/u/bin")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.startGuardFiles(ctx)
	return dir
}

func applyPolicy(t *testing.T, s *server, doc string) int64 {
	t.Helper()
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	return d.GetRevision()
}

func readSnapshot(t *testing.T, dir string) spaces.GuardSnapshot {
	t.Helper()
	snap, err := spaces.ReadGuardSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func bashInput(session, cwd, cmd string) spaces.GuardInput {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: session, Tool: "Bash", Input: in, Cwd: cwd}
}

// TestGuardSnapshotFollowsPolicy: orchd writes the snapshot at start, again
// when a policy is applied, and removes it without a spaces engine.
func TestGuardSnapshotFollowsPolicy(t *testing.T) {
	s := spacesServer(t)
	withEnforcement(t, s)
	dir := withGuardFiles(t, s)
	snap := readSnapshot(t, dir)
	if snap.Policy != nil || snap.Revision != 0 || snap.Broken || snap.Home != "/home/u" ||
		snap.Guard.State != spaces.SwitchBlockOtherJobs {
		t.Fatalf("snapshot at start = %+v", snap)
	}
	// The snapshot carries the whole self-protection (RC-110): orch's
	// directories, the guard's own, harness settings and paths protected
	// below every directory.
	prot := snap.Protection
	for _, want := range []string{"/home/u/.config/orch", filepath.Dir(dir), dir, "/home/u/.claude/settings*", "/home/u/bin/orch-hook"} {
		if !slices.Contains(prot.Write, want) {
			t.Errorf("snapshot protection %v lacks %s", prot.Write, want)
		}
	}
	if !slices.Contains(prot.Anywhere, filepath.Join(".git", "hooks")) || len(prot.Read) == 0 {
		t.Errorf("snapshot protection = %+v", prot)
	}
	rev := applyPolicy(t, s, guardJobsPolicy)
	snap = readSnapshot(t, dir)
	if snap.Policy == nil || len(snap.Policy.Spaces) != 2 || snap.Revision != rev {
		t.Fatalf("snapshot after apply = %+v", snap)
	}
	// Reason: new servers, not s changed in place: s's pin sync still runs.
	(&server{guard: s.guard}).writeGuardSnapshot()
	if _, err := spaces.ReadGuardSnapshot(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot without spaces: %v", err)
	}
	// Without a writer nothing is written.
	none := &server{}
	none.writeGuardSnapshot()
	none.writeGuardPin(sessionPin{SessionID: "x"})
	none.setSwitches(spaces.DefaultEnforcement())
	none.syncGuardPins(context.Background())
	none.startGuardFiles(context.Background())
}

// TestGuardSnapshotRemovedWhenWriteFails: a snapshot orchd cannot rewrite
// is removed rather than left stale, so the hook refuses until it is
// written again.
func TestGuardSnapshotRemovedWhenWriteFails(t *testing.T) {
	s := spacesServer(t)
	dir := withGuardFiles(t, s)
	readSnapshot(t, dir)
	old := writeSnapshotFile
	t.Cleanup(func() { writeSnapshotFile = old })
	writeSnapshotFile = func(string, spaces.GuardSnapshot) error { return errors.New("disk full") }
	s.writeGuardSnapshot()
	if _, err := spaces.ReadGuardSnapshot(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale snapshot kept: %v", err)
	}
}

// TestGuardSwitchTakesEffectNextCall is SP-041.2.a2: switching the guard
// off through SP-070 changes the decision of the next tool call of a
// running session, and a timed switch reverts on time even before orchd
// rewrites the snapshot.
func TestGuardSwitchTakesEffectNextCall(t *testing.T) {
	s := spacesServer(t)
	rig := withEnforcement(t, s)
	dir := withGuardFiles(t, s)
	applyPolicy(t, s, guardJobsPolicy)
	s.writeGuardPin(sessionPin{SessionID: "sess", Space: "work"})
	files := spaces.GuardFiles{Dir: dir, DB: filepath.Join(t.TempDir(), "none.db"), Now: rig.clock.now}
	call := bashInput("sess", "/home/u/code/work", "cat /home/u/code/other/notes")
	ctx := context.Background()

	if r := files.Decide(ctx, call); r.Verdict != spaces.Refuse || r.Rule != spaces.RuleOtherJobPath || !r.Enforced {
		t.Fatalf("guard on: %s (%s)", r.Verdict, r.Rule)
	}
	setSwitch(t, s, "guard", spaces.SwitchOff, 0)
	if r := files.Decide(ctx, call); r.Verdict != spaces.Allow || r.Would != spaces.Refuse || r.Enforced || r.Mode != spaces.GuardOff {
		t.Fatalf("guard off: %s would %s in %s", r.Verdict, r.Would, r.Mode)
	}
	setSwitch(t, s, "guard", spaces.SwitchStrict, 0)
	if r := files.Decide(ctx, call); r.Verdict != spaces.Refuse || r.Mode != spaces.GuardStrict {
		t.Fatalf("guard strict: %s in %s", r.Verdict, r.Mode)
	}
	setSwitch(t, s, "guard", spaces.SwitchLog, 900)
	if r := files.Decide(ctx, call); r.Verdict != spaces.Allow || r.Mode != spaces.GuardLog {
		t.Fatalf("guard log for 15m: %s in %s", r.Verdict, r.Mode)
	}
	rig.clock.advance(15 * time.Minute)
	// The snapshot still says log; its timer has run out.
	if snap := readSnapshot(t, dir); snap.Guard.State != spaces.SwitchLog {
		t.Fatalf("snapshot rewritten before the revert: %+v", snap.Guard)
	}
	if r := files.Decide(ctx, call); r.Verdict != spaces.Refuse || r.Mode != spaces.GuardStrict {
		t.Fatalf("after the timer, before orchd's revert: %s in %s", r.Verdict, r.Mode)
	}
	s.enforcement.expire(ctx)
	if snap := readSnapshot(t, dir); snap.Guard.State != spaces.SwitchStrict || !snap.Guard.Until.IsZero() {
		t.Fatalf("snapshot after the revert: %+v", snap.Guard)
	}
}

// TestGuardPinFiles: PinSession writes the session's pin file, and the sync
// removes the files of sessions whose harness process exited.
func TestGuardPinFiles(t *testing.T) {
	s, dirs := pinsServer(t)
	dir := withGuardFiles(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinSession(inCodex, pinReq("sess-b", dirs[1], "")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sess-a", "sess-b"} {
		if p, err := spaces.ReadGuardPin(dir, id); err != nil || p.SessionID != id {
			t.Fatalf("pin file of %s: %+v, %v", id, p, err)
		}
	}
	if _, err := s.pins.db.Exec(`UPDATE session_pins SET ended_at = ?, exited_at = ? WHERE session_id = 'sess-b'`,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	stale := spaces.GuardPinPath(dir, "gone")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(spaces.GuardPinPath(dir, "sess-a")); err != nil {
		t.Fatal(err)
	}
	s.syncGuardPins(context.Background())
	if _, err := spaces.ReadGuardPin(dir, "sess-a"); err != nil {
		t.Errorf("live session's pin file not rewritten: %v", err)
	}
	for _, id := range []string{"sess-b", "gone"} {
		if _, err := os.Stat(spaces.GuardPinPath(dir, id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pin file of %s kept: %v", id, err)
		}
	}
	// A pin file that cannot be written is logged, not fatal.
	s.writeGuardPin(sessionPin{SessionID: "../bad"})
}

// TestHookDecisionRecorded: the command log stores the decision the hook
// reports: an enforced verdict as the decision with its rule, one not
// enforced as the verdict the guard would have given (RC-86).
func TestHookDecisionRecorded(t *testing.T) {
	cases := []struct {
		verdict, rule string
		enforced      bool
		decision      string
		storedRule    string
	}{
		{"refuse", "other_job_path", true, toollog.DecisionRefused, "other_job_path"},
		{"allow", "within_space", true, decisionAllowed, "within_space"},
		{"refuse", "other_job_repo", false, decisionLogged, "would refuse (other_job_repo)"},
		{"refuse", "/home/u/code/other", true, toollog.DecisionRefused, "unknown"},
		{"maybe", "x", true, decisionLogged, ""},
		{"", "", false, decisionLogged, ""},
	}
	session, process := sessionOwner("s"), toollog.Owner{Kind: toollog.OwnerProcess, ID: "1:2"}
	for _, c := range []struct {
		req   *orchdv1.RecordToolEventRequest
		owner toollog.Owner
		want  bool
	}{
		{&orchdv1.RecordToolEventRequest{GuardVerdict: "refuse", GuardEnforced: true}, session, true},
		{&orchdv1.RecordToolEventRequest{GuardVerdict: "refuse"}, session, false},
		{&orchdv1.RecordToolEventRequest{GuardVerdict: "allow", GuardEnforced: true}, session, false},
		{&orchdv1.RecordToolEventRequest{GuardVerdict: "refuse", GuardEnforced: true}, process, false},
	} {
		if got := guardRefused(c.req, c.owner); got != c.want {
			t.Errorf("guardRefused(%v, %s) = %v", c.req, c.owner.Kind, got)
		}
	}
	for _, c := range cases {
		r := toollog.Record{Decision: decisionLogged}
		applyHookDecision(&r, &orchdv1.RecordToolEventRequest{GuardVerdict: c.verdict, GuardRule: c.rule, GuardEnforced: c.enforced})
		if r.Decision != c.decision || r.Rule != c.storedRule {
			t.Errorf("%+v: stored %q %q", c, r.Decision, r.Rule)
		}
	}
}

// TestRefusalStoredWhenShareFull is SP-041.1.a4 (D-37): with a session's
// share of the queue full of ordinary events, a call the guard refuses is
// still stored with its decision, and the ordinary events past the share
// are dropped with a gap record.
func TestRefusalStoredWhenShareFull(t *testing.T) {
	s, dirs := pinsServer(t)
	withToolLog(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	ctx := pinnedCtx(t, s, "sess-a")
	ev := func(req *orchdv1.RecordToolEventRequest) toolEvent {
		return toolEvent{caller: callerOf(ctx), req: req, received: time.Now()}
	}
	key, _, _ := s.toolLogKey(ev(bashReq("sess-a", dirs[0], "ls")))
	// Hold the writer on its first event, so nothing leaves the queue.
	release, started := make(chan struct{}), make(chan struct{})
	s.toolLog.writer.Enqueue(key, 1, func() toollog.Record { close(started); <-release; return toollog.Record{Event: "pre-tool-use"} })
	<-started
	const flood = toollog.DefaultQueue/4 + 50
	for range flood {
		s.logToolEvent(ev(bashReq("sess-a", dirs[0], "ls")))
	}
	dropped := s.toolLog.writer.Dropped()
	if dropped == 0 {
		t.Fatal("the flood did not fill the session's share")
	}
	refused := bashReq("sess-a", dirs[0], "cat /elsewhere/secret")
	refused.GuardVerdict, refused.GuardRule, refused.GuardEnforced = "refuse", "other_job_path", true
	s.logToolEvent(ev(refused))
	if s.toolLog.writer.Dropped() != dropped {
		t.Fatal("the refusal was dropped")
	}
	close(release)
	flushToolLog(t, s)
	var refusals, gaps int
	var lost int64
	for _, r := range records(t, s, sessionOwner("sess-a")) {
		switch {
		case r.Decision == toollog.DecisionRefused:
			refusals++
			if r.Rule != "other_job_path" || r.Command != "cat /elsewhere/secret" {
				t.Errorf("refusal stored as %+v", r)
			}
		case r.Kind == toollog.KindGap:
			gaps++
			lost += r.Dropped
		}
	}
	if refusals != 1 || gaps == 0 || lost != dropped {
		t.Errorf("%d refusals, %d gap records counting %d dropped; want 1, >0, %d", refusals, gaps, lost, dropped)
	}
}

// TestPeerReusedPIDGetsNoSpace is SP-041.1.a5 (D-44): a connection whose
// peer PID now names another process than the one that connected (it
// handed its socket on and exited, and a harness in another space took the
// PID) is not resolved to that harness's session; it gets no space.
func TestPeerReusedPIDGetsNoSpace(t *testing.T) {
	s, dirs := pinsServer(t)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	// Process 900 runs under the pinned claude 400.
	parent := func(pid int) (int, error) {
		return map[int]int{900: 400, 400: 1}[pid], nil
	}
	program := func(pid int) (procstat.Program, error) {
		if pid == 400 {
			return procstat.Program{Name: "claude"}, nil
		}
		return procstat.Program{Name: "zsh"}, nil
	}
	started := func(pid int) (time.Time, error) {
		if pid == 400 {
			return claudeStart, nil
		}
		return time.Time{}, errors.New("not tracked")
	}
	res := &callerResolver{pins: s.pins}
	for _, tc := range []struct {
		name     string
		samePeer func(net.Conn, int) error
		want     callerKind
	}{
		{"the PID still names the peer", func(net.Conn, int) error { return nil }, callerSession},
		{"the PID was reused", func(net.Conn, int) error { return errPeerReplaced }, callerNone},
		{"the token cannot be read", func(net.Conn, int) error { return errors.New("no token") }, callerNone},
	} {
		creds := peerCredentials{self: -1, parent: parent, program: program, started: started,
			peerPID: func(net.Conn) (int, error) { return 900, nil }, samePeer: tc.samePeer}
		a := creds.identify(nil)
		got := res.resolve(peer.NewContext(context.Background(), &peer.Peer{AuthInfo: a}))
		if got.kind != tc.want || (tc.want == callerSession && got.pin.SessionID != "sess-a") {
			t.Errorf("%s: resolved to %s %q (%s)", tc.name, got.kind, got.pin.SessionID, got.why)
		}
		if who, queued := queuedCaller(peer.NewContext(context.Background(), &peer.Peer{AuthInfo: a})); tc.want == callerNone && (!queued || who != "unidentified process") {
			t.Errorf("%s: operator check sees %q, %v", tc.name, who, queued)
		}
	}
}

// TestConnSamePeerRealSocket checks the kernel token on a real socket:
// the connecting process matches itself and no other PID.
func TestConnSamePeerRealSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "p.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := lis.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := connSamePeer(conn, os.Getpid()); err != nil {
		t.Errorf("own process: %v", err)
	}
	if err := connSamePeer(conn, os.Getppid()); !errors.Is(err, errPeerReplaced) {
		t.Errorf("another PID: %v", err)
	}
	if err := connSamePeer(nil, 1); err == nil {
		t.Error("a connection without a socket passed")
	}
	// identify with the real token check passes for this process.
	creds := peerCredentials{self: -1, parent: procstat.Parent}
	if a := creds.identify(conn); a.err != nil || a.pid != os.Getpid() {
		t.Errorf("identify = pid %d, %v", a.pid, a.err)
	}
}

// shortTempDir is a temporary directory with a path short enough for a
// unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "orch-pt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// TestGuardDirPath: the guard's files live in the data directory.
func TestGuardDirPath(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", "/x/data")
	if paths.GuardDir() != "/x/data/guard" {
		t.Errorf("GuardDir = %s", paths.GuardDir())
	}
}
