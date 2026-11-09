package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// procTable is a fake kernel process table for pin tests: start times and
// working directories by PID.
type procTable struct {
	started map[int]time.Time
	cwd     map[int]string
}

func (t procTable) startTime(pid int) (time.Time, error) {
	if s, ok := t.started[pid]; ok {
		return s, nil
	}
	return time.Time{}, errors.New("no such process")
}

func (t procTable) workdir(pid int) (string, error) {
	if c, ok := t.cwd[pid]; ok {
		return c, nil
	}
	return "", errors.New("no such process")
}

var (
	claudeStart = time.Unix(1_790_000_000, 123_456_789)
	codexStart  = time.Unix(1_790_000_030, 0)
)

// withPins gives s a pin registry over the fake processes in tbl: claude 400
// and codex 410, each started in its own directory.
func withPins(t *testing.T, s *server, cwd400, cwd410 string) procTable {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tbl := procTable{
		started: map[int]time.Time{400: claudeStart, 410: codexStart},
		cwd:     map[int]string{400: cwd400, 410: cwd410},
	}
	s.pins = newPinRegistry(conn)
	s.pins.started, s.pins.cwd = tbl.startTime, tbl.workdir
	// Both harnesses started a minute ago, inside the new-pin window.
	s.pins.now = func() time.Time { return codexStart.Add(time.Minute) }
	return tbl
}

// pinsServer is a server with no spaces policy and a pin registry; dirs are
// two existing directories with symlinks resolved.
func pinsServer(t *testing.T) (*server, [2]string) {
	t.Helper()
	s := spacesServer(t)
	var dirs [2]string
	for i := range dirs {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dirs[i] = d
	}
	withPins(t, s, dirs[0], dirs[1])
	return s, dirs
}

// inClaude is a hook process (pid 900) under claude 400; inCodex is under
// codex 410.
var (
	inClaude = callerCtx(callerAuth{pid: 900, harness: "claude", harnessProc: procstat.Identity{PID: 400, Started: claudeStart}})
	inCodex  = callerCtx(callerAuth{pid: 910, harness: "codex", harnessProc: procstat.Identity{PID: 410, Started: codexStart}})
)

func pinReq(id, cwd, parent string) *orchdv1.PinSessionRequest {
	return &orchdv1.PinSessionRequest{SessionId: id, Cwd: cwd, ParentSessionId: parent}
}

// TestPinSessionKeepsFirstPin is SP-010.1.a1: a second PinSession for the same
// session with another cwd leaves the pin unchanged and returns it.
func TestPinSessionKeepsFirstPin(t *testing.T) {
	s, dirs := pinsServer(t)
	first, err := s.PinSession(inClaude, pinReq("sess-1", dirs[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetSessionId() != "sess-1" || first.GetHarness() != "claude" || first.GetHarnessPid() != 400 ||
		first.GetHarnessStarted() != claudeStart.UnixNano() || first.GetFirstCwd() != dirs[0] ||
		first.GetPinnedAt() == 0 || first.GetEndedAt() != 0 || first.GetInheritedFrom() != "" {
		t.Fatalf("first pin = %v", first)
	}
	// The harness's working directory changed (as a cd in its shell would
	// not, but a chdir might); the pin still does not.
	s.pins.cwd = procTable{cwd: map[int]string{400: dirs[1]}}.workdir
	again, err := s.PinSession(inClaude, pinReq("sess-1", dirs[1], ""))
	if err != nil {
		t.Fatal(err)
	}
	if again.String() != first.String() {
		t.Fatalf("second PinSession = %v, want the original %v", again, first)
	}
	stored, ok, err := s.pins.get(context.Background(), "sess-1")
	if err != nil || !ok || stored.FirstCwd != dirs[0] {
		t.Fatalf("stored pin = %+v, %v, %v", stored, ok, err)
	}
}

// TestPinSessionValidatesIDs is SP-010.1.a2.
func TestPinSessionValidatesIDs(t *testing.T) {
	s, dirs := pinsServer(t)
	bad := []string{"", "a/b", "../x", "a b", "a.b", "é", "a\nb", strings.Repeat("a", 129), "sess;rm"}
	for _, id := range bad {
		if _, err := s.PinSession(inClaude, pinReq(id, dirs[0], "")); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("PinSession(%q) = %v, want InvalidArgument", id, err)
		}
		if id != "" {
			if _, err := s.PinSession(inClaude, pinReq("ok", dirs[0], id)); grpcstatus.Code(err) != codes.InvalidArgument {
				t.Errorf("PinSession(parent %q) = %v, want InvalidArgument", id, err)
			}
		}
		if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: id}); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("EndSession(%q) = %v, want InvalidArgument", id, err)
		}
	}
	for _, id := range []string{strings.Repeat("a", 128), "01a1bbb9-30be-72f3-91d2-6ba43c1530d0", "A_b-9"} {
		if _, err := s.PinSession(inClaude, pinReq(id, dirs[0], "")); err != nil {
			t.Errorf("PinSession(%q) = %v", id, err)
		}
	}
	var n int
	if err := s.pins.db.QueryRow(`SELECT count(*) FROM session_pins`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("pins stored = %d, %v; want only the 3 valid IDs", n, err)
	}
}

// TestPinSessionHarnessFromProcessTree is SP-010.1.a3: orchd takes the
// harness PID and start time from the connection's process tree, never from
// the request (which has no field for them).
func TestPinSessionHarnessFromProcessTree(t *testing.T) {
	s, dirs := pinsServer(t)
	// The hook (901) runs under a shell (902) under claude (400) under a
	// terminal (30). Another claude (401) is unrelated.
	progs := map[int]procstat.Program{
		901: {Name: "orch-hook"}, 902: {Name: "zsh"}, 400: {Name: "claude"}, 30: {Name: "Terminal"}, 401: {Name: "claude"},
	}
	parents := map[int]int{901: 902, 902: 400, 400: 30, 30: 1, 401: 30}
	creds := peerCredentials{
		self:    1000,
		parent:  func(pid int) (int, error) { return parents[pid], nil },
		program: func(pid int) (procstat.Program, error) { return progs[pid], nil },
		started: procTable{started: map[int]time.Time{400: claudeStart, 401: codexStart}}.startTime,
		peerPID: func(net.Conn) (int, error) { return 901, nil },
	}
	a := creds.identify(nil)
	if a.harness != "claude" || a.harnessErr != nil || a.harnessProc.PID != 400 || !a.harnessProc.Started.Equal(claudeStart) {
		t.Fatalf("identify = %+v", a)
	}
	pin, err := s.PinSession(callerCtx(a), pinReq("sess-tree", dirs[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	if pin.GetHarnessPid() != 400 || pin.GetHarnessStarted() != claudeStart.UnixNano() || pin.GetHarness() != "claude" {
		t.Fatalf("pin = %v, want claude 400 from the process tree", pin)
	}
	// The request carries no PID: proto has only the ID, cwd and parent.
	fields := (&orchdv1.PinSessionRequest{}).ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		if name := string(fields.Get(i).Name()); strings.Contains(name, "pid") || strings.Contains(name, "harness") {
			t.Errorf("PinSessionRequest has a field %q a caller could fill", name)
		}
	}

	// A caller with no harness above it, one orchd could not walk, and one
	// whose start time was never read cannot pin.
	for name, ctx := range map[string]context.Context{
		"operator":       operatorCtx,
		"walk failed":    callerCtx(callerAuth{pid: 7, harnessErr: errors.New("parent unreadable")}),
		"peer unknown":   callerCtx(callerAuth{err: errors.New("no peer")}),
		"no start time":  callerCtx(callerAuth{pid: 7, harness: "claude", harnessProc: procstat.Identity{PID: 400}}),
		"no auth at all": context.Background(),
	} {
		if _, err := s.PinSession(ctx, pinReq("sess-x", dirs[0], "")); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: PinSession = %v, want PermissionDenied", name, err)
		}
	}
	// A start time that unreadable at accept is a harness error.
	creds.started = procTable{}.startTime
	if a := creds.identify(nil); a.harnessErr == nil {
		t.Errorf("identify with an unreadable start time = %+v", a)
	}
	// A harness that exited since the hook connected (its PID now names a
	// later process) is refused.
	replaced := callerCtx(callerAuth{pid: 7, harness: "claude", harnessProc: procstat.Identity{PID: 400, Started: claudeStart.Add(-time.Hour)}})
	if _, err := s.PinSession(replaced, pinReq("sess-y", dirs[0], "")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("PinSession from a replaced harness = %v", err)
	}
}

// TestPinSessionChecksCwd: a new pin's directory must be the harness
// process's own, so a process in an unpinned harness cannot pick its space.
func TestPinSessionChecksCwd(t *testing.T) {
	s, dirs := pinsServer(t)
	for _, cwd := range []string{dirs[1], "relative/dir", "", filepath.Join(dirs[0], "missing")} {
		if _, err := s.PinSession(inClaude, pinReq("sess-cwd", cwd, "")); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("PinSession(cwd %q) = %v, want FailedPrecondition", cwd, err)
		}
	}
	// A symlink to the harness's directory names the same place.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dirs[0], link); err != nil {
		t.Fatal(err)
	}
	pin, err := s.PinSession(inClaude, pinReq("sess-cwd", link, ""))
	if err != nil || pin.GetFirstCwd() != dirs[0] {
		t.Fatalf("PinSession(symlink) = %v, %v", pin, err)
	}
	// An unreadable harness working directory refuses the pin.
	s.pins.cwd = procTable{}.workdir
	if _, err := s.PinSession(inCodex, pinReq("sess-cwd2", dirs[1], "")); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("PinSession with an unreadable harness cwd = %v, want Internal", err)
	}
}

// TestSessionStartSourcesKeepPin is SP-010.2.a1: SessionStart for resume,
// clear and compact (and a repeated startup) keeps the existing pin. Claude
// Code's /clear starts a new session ID in the same process, which gets that
// process's pin; resume and compact repeat the session's own ID.
func TestSessionStartSourcesKeepPin(t *testing.T) {
	s, dirs := pinsServer(t)
	orig, err := s.PinSession(inClaude, pinReq("sess-life", dirs[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	// From here the harness reports another directory each time.
	s.pins.cwd = procTable{cwd: map[int]string{400: dirs[1]}}.workdir
	cases := []struct {
		source, id string
	}{
		{"startup", "sess-life"},
		{"resume", "sess-life"},
		{"compact", "sess-life"},
		{"clear", "sess-cleared"},
	}
	for _, tt := range cases {
		t.Run(tt.source, func(t *testing.T) {
			pin, err := s.PinSession(inClaude, pinReq(tt.id, dirs[1], ""))
			if err != nil {
				t.Fatal(err)
			}
			if pin.GetFirstCwd() != orig.GetFirstCwd() || pin.GetSpace() != orig.GetSpace() ||
				pin.GetHarnessPid() != orig.GetHarnessPid() || pin.GetPolicyRevision() != orig.GetPolicyRevision() {
				t.Fatalf("%s pin = %v, want %v", tt.source, pin, orig)
			}
			if tt.id != "sess-life" && pin.GetInheritedFrom() != "sess-life" {
				t.Fatalf("%s pin inherited from %q", tt.source, pin.GetInheritedFrom())
			}
		})
	}
	// Even after the first session ended, the process keeps its pin.
	if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-life"}); err != nil {
		t.Fatal(err)
	}
	pin, err := s.PinSession(inClaude, pinReq("sess-after-end", dirs[1], ""))
	if err != nil || pin.GetFirstCwd() != dirs[0] || pin.GetInheritedFrom() != "sess-life" {
		t.Fatalf("pin after the first session ended = %v, %v", pin, err)
	}
	// Resumed in another process, the pin is not handed to it.
	if _, err := s.PinSession(inCodex, pinReq("sess-life", dirs[1], "")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("PinSession from another process = %v, want PermissionDenied", err)
	}
}

// TestSubagentInheritsParentPin is SP-010.2.a2.
func TestSubagentInheritsParentPin(t *testing.T) {
	s, dirs := pinsServer(t)
	parent, err := s.PinSession(inClaude, pinReq("parent", dirs[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	// The subagent asks for another directory; it gets its parent's pin.
	sub, err := s.PinSession(inClaude, pinReq("sub-1", dirs[1], "parent"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.GetSessionId() != "sub-1" || sub.GetInheritedFrom() != "parent" || sub.GetFirstCwd() != parent.GetFirstCwd() ||
		sub.GetSpace() != parent.GetSpace() || sub.GetHarnessPid() != parent.GetHarnessPid() {
		t.Fatalf("subagent pin = %v, parent %v", sub, parent)
	}
	// A subagent of the subagent still names the root session.
	subsub, err := s.PinSession(inClaude, pinReq("sub-2", dirs[1], "sub-1"))
	if err != nil || subsub.GetInheritedFrom() != "parent" {
		t.Fatalf("nested subagent pin = %v, %v", subsub, err)
	}
	// It cannot set its own later either.
	again, err := s.PinSession(inClaude, pinReq("sub-1", dirs[1], ""))
	if err != nil || again.GetFirstCwd() != dirs[0] {
		t.Fatalf("subagent re-pin = %v, %v", again, err)
	}
	// A parent with no pin, or one pinned to another harness process, gives
	// the subagent nothing.
	if _, err := s.PinSession(inCodex, pinReq("sub-3", dirs[1], "nobody")); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("subagent of an unpinned parent = %v, want FailedPrecondition", err)
	}
	if _, err := s.PinSession(inCodex, pinReq("sub-4", dirs[1], "parent")); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("subagent claiming another process's parent = %v, want PermissionDenied", err)
	}
	for _, id := range []string{"sub-3", "sub-4"} {
		if _, ok, _ := s.pins.get(context.Background(), id); ok {
			t.Errorf("%s was pinned", id)
		}
	}
}

// TestEndSessionKeepsPin is SP-010.2.a3: SessionEnd sets ended_at and the row
// stays for retention.
func TestEndSessionKeepsPin(t *testing.T) {
	s, dirs := pinsServer(t)
	now := claudeStart.Add(time.Minute).UTC()
	s.pins.now = func() time.Time { return now }
	if _, err := s.PinSession(inClaude, pinReq("sess-end", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	resp, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-end"})
	if err != nil || resp.GetEndedAt() != now.Unix() {
		t.Fatalf("EndSession = %v, %v", resp, err)
	}
	// A second end keeps the first time.
	now = now.Add(time.Hour)
	if resp, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-end"}); err != nil || resp.GetEndedAt() != now.Add(-time.Hour).Unix() {
		t.Fatalf("second EndSession = %v, %v", resp, err)
	}
	p, ok, err := s.pins.get(context.Background(), "sess-end")
	if err != nil || !ok || p.EndedAt.Unix() != now.Add(-time.Hour).Unix() || p.FirstCwd != dirs[0] {
		t.Fatalf("pin after end = %+v, %v, %v", p, ok, err)
	}
	if pin, err := s.PinSession(inClaude, pinReq("sess-end", dirs[0], "")); err != nil || pin.GetEndedAt() == 0 {
		t.Fatalf("pin after end = %v, %v", pin, err)
	}
	// Another process cannot end it, nobody can end a session with no pin,
	// and a caller outside a harness cannot end anything.
	if _, err := s.EndSession(inCodex, &orchdv1.EndSessionRequest{SessionId: "sess-end"}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("EndSession from another process = %v", err)
	}
	if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "nobody"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("EndSession(unpinned) = %v", err)
	}
	if _, err := s.EndSession(operatorCtx, &orchdv1.EndSessionRequest{SessionId: "sess-end"}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("EndSession(operator) = %v", err)
	}
}

// TestPinWithoutSpaces is SP-010.3.a1 on orchd's side: with no spaces
// policy, a pin records space "" and revision 0.
func TestPinWithoutSpaces(t *testing.T) {
	s, dirs := pinsServer(t)
	pin, err := s.PinSession(inClaude, pinReq("sess-none", dirs[0], ""))
	if err != nil || pin.GetSpace() != "" || pin.GetPolicyRevision() != 0 || pin.GetMainCheckout() != "" {
		t.Fatalf("pin without spaces = %v, %v", pin, err)
	}
	// Nor does a server without a spaces engine fail.
	s.spaces = nil
	if pin, err := s.PinSession(inCodex, pinReq("sess-none-2", dirs[1], "")); err != nil || pin.GetSpace() != "" {
		t.Fatalf("pin without a spaces engine = %v, %v", pin, err)
	}
}

// TestPinPlacesSession: under an applied policy the pin records the space a
// session started there writes to, the revision and the main checkout.
func TestPinPlacesSession(t *testing.T) {
	s, code, _ := placementServer(t)
	app := code + "/company1/app"
	withPins(t, s, app, code+"/willesq")
	pin, err := s.PinSession(inClaude, pinReq("sess-acme", app, ""))
	if err != nil {
		t.Fatal(err)
	}
	if pin.GetSpace() != "acme" || pin.GetPolicyRevision() == 0 || pin.GetMainCheckout() != app {
		t.Fatalf("pin in acme = %v", pin)
	}
	// D-22: a session under a public root writes to public_root_sessions.
	s.pins.cwd = procTable{cwd: map[int]string{410: code}}.workdir
	pin, err = s.PinSession(inCodex, pinReq("sess-oss", code, ""))
	if err != nil || pin.GetSpace() != "personal" {
		t.Fatalf("pin under the public root = %v, %v", pin, err)
	}
}

// TestPinsUnavailable: without a registry both RPCs say so.
func TestPinsUnavailable(t *testing.T) {
	s := &server{}
	if _, err := s.PinSession(inClaude, pinReq("a", "/", "")); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("PinSession = %v", err)
	}
	if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "a"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("EndSession = %v", err)
	}
}

// TestPlaceSessionBrokenPolicy: when the applied policy does not verify, a
// new pin is quarantined rather than shared.
func TestPlaceSessionBrokenPolicy(t *testing.T) {
	s, _, _ := placementServer(t)
	if _, err := s.spaces.Current(); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(`UPDATE spaces_revisions SET doc = doc || ' '`); err != nil {
		t.Fatal(err)
	}
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }, LookupOrg: orgNodes{}.lookup})
	if err != nil {
		t.Fatal(err)
	}
	s.spaces = e
	space, rev, err := s.placeSession(context.Background(), "/anywhere")
	if err != nil || space != spaces.Quarantine || rev != 0 {
		t.Fatalf("placeSession under a broken policy = %q, %d, %v", space, rev, err)
	}
}

// TestQueuedTasksCannotPin: a queued task's session is identified by its task
// record, so its hooks are refused the pin RPCs.
func TestQueuedTasksCannotPin(t *testing.T) {
	task := callerCtx(callerAuth{pid: 7, spawned: true, harness: "claude"})
	for _, m := range []string{orchdv1.Orchd_PinSession_FullMethodName, orchdv1.Orchd_EndSession_FullMethodName} {
		if err := refuseQueued(task, m); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("refuseQueued(%s) = %v", m, err)
		}
		if err := refuseQueued(inClaude, m); err != nil {
			t.Errorf("refuseQueued(%s) from an interactive session = %v", m, err)
		}
	}
}

// TestNestedHarnessKeepsOuterPin is the audit's high finding: a harness an
// agent starts inside its session (a real claude in another directory, or
// any program it names claude) keeps the outer session's pin and ignores its
// own cwd; inside an unpinned session it gets no pin at all.
func TestNestedHarnessKeepsOuterPin(t *testing.T) {
	s, dirs := pinsServer(t)
	outer, err := s.PinSession(inClaude, pinReq("outer", dirs[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	// 420 is a claude the agent started in dirs[1]; 430 is /bin/sh copied
	// to /tmp/x/claude, also in dirs[1]. Both run under claude 400.
	innerStart := claudeStart.Add(30 * time.Second)
	tbl := procTable{
		started: map[int]time.Time{400: claudeStart, 410: codexStart, 420: innerStart, 430: innerStart},
		cwd:     map[int]string{400: dirs[0], 420: dirs[1], 430: dirs[1]},
	}
	s.pins.started, s.pins.cwd = tbl.startTime, tbl.workdir
	progs := map[int]procstat.Program{
		921: {Name: "orch-hook"}, 420: {Name: "claude"}, 931: {Name: "orch-hook"}, 430: {Name: "claude", Path: "/tmp/x/claude"},
		902: {Name: "zsh"}, 400: {Name: "claude"}, 30: {Name: "Terminal"},
		911: {Name: "orch-hook"}, 410: {Name: "codex"},
	}
	parents := map[int]int{921: 420, 420: 902, 931: 430, 430: 902, 902: 400, 400: 30, 30: 1, 911: 410, 410: 30}
	creds := peerCredentials{
		self:    1000,
		parent:  func(pid int) (int, error) { return parents[pid], nil },
		program: func(pid int) (procstat.Program, error) { return progs[pid], nil },
		started: tbl.startTime,
	}
	hookOf := func(pid int) context.Context {
		creds.peerPID = func(net.Conn) (int, error) { return pid, nil }
		return callerCtx(creds.identify(nil))
	}
	for _, tt := range []struct {
		name, id      string
		hook, harness int
	}{
		{"real nested claude", "inner-real", 921, 420},
		{"fake claude", "inner-fake", 931, 430},
	} {
		ctx := hookOf(tt.hook)
		pin, err := s.PinSession(ctx, pinReq(tt.id, dirs[1], ""))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if pin.GetFirstCwd() != outer.GetFirstCwd() || pin.GetSpace() != outer.GetSpace() ||
			pin.GetInheritedFrom() != "outer" || pin.GetHarnessPid() != int64(tt.harness) {
			t.Fatalf("%s pin = %v, want outer's space and cwd on harness %d", tt.name, pin, tt.harness)
		}
		// The nested harness can end its own pin, not the outer one.
		if _, err := s.EndSession(ctx, &orchdv1.EndSessionRequest{SessionId: "outer"}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s ended the outer session: %v", tt.name, err)
		}
		if _, err := s.EndSession(ctx, &orchdv1.EndSessionRequest{SessionId: tt.id}); err != nil {
			t.Fatalf("%s EndSession = %v", tt.name, err)
		}
	}
	// Nested in codex 410, which has no pin: refused.
	parents[421] = 410
	progs[421] = procstat.Program{Name: "claude"}
	tbl.started[421], tbl.cwd[421] = innerStart, dirs[1]
	parents[922], progs[922] = 421, procstat.Program{Name: "orch-hook"}
	if _, err := s.PinSession(hookOf(922), pinReq("inner-orphan", dirs[1], "")); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nested in an unpinned session = %v, want FailedPrecondition", err)
	}
	// An outer harness orchd cannot read refuses the caller.
	creds.started = procTable{started: map[int]time.Time{420: innerStart}}.startTime
	if a := creds.identify(nil); a.harnessErr == nil {
		t.Errorf("identify with an unreadable outer harness = %+v", a)
	}
	creds.parent = func(pid int) (int, error) {
		if pid == 420 {
			return 0, errors.New("gone")
		}
		return parents[pid], nil
	}
	creds.peerPID = func(net.Conn) (int, error) { return 921, nil }
	if a := creds.identify(nil); a.harnessErr == nil {
		t.Errorf("identify with an unreadable parent = %+v", a)
	}
}

// TestNewPinOnlyWhileHarnessIsNew: a harness that started more than
// newPinWindow ago may have had its working directory moved, so it takes no
// new pin; it keeps one it already has.
func TestNewPinOnlyWhileHarnessIsNew(t *testing.T) {
	s, dirs := pinsServer(t)
	if _, err := s.PinSession(inCodex, pinReq("early", dirs[1], "")); err != nil {
		t.Fatal(err)
	}
	s.pins.now = func() time.Time { return claudeStart.Add(newPinWindow + time.Second) }
	if _, err := s.PinSession(inClaude, pinReq("late", dirs[0], "")); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("late new pin = %v, want FailedPrecondition", err)
	}
	if _, err := s.PinSession(inCodex, pinReq("early", dirs[1], "")); err != nil {
		t.Fatalf("existing pin after the window = %v", err)
	}
	if pin, err := s.PinSession(inCodex, pinReq("cleared", dirs[1], "")); err != nil || pin.GetInheritedFrom() != "early" {
		t.Fatalf("same-process pin after the window = %v, %v", pin, err)
	}
}

// TestDetachedHarnessGetsNoNewPin is the re-audit's finding: a process named
// claude that an agent detached from its session (its parent exited, so
// launchd adopted it) has no harness above it but takes no new pin.
func TestDetachedHarnessGetsNoNewPin(t *testing.T) {
	s, dirs := pinsServer(t)
	tbl := procTable{started: map[int]time.Time{440: claudeStart}, cwd: map[int]string{440: dirs[1]}}
	s.pins.started, s.pins.cwd = tbl.startTime, tbl.workdir
	creds := peerCredentials{
		self:   1000,
		parent: func(pid int) (int, error) { return map[int]int{941: 440, 440: 1}[pid], nil },
		program: func(pid int) (procstat.Program, error) {
			if pid == 440 {
				return procstat.Program{Name: "sh", Path: "/bin/sh", Args: []string{"claude", "-c", "orch-hook session-start"}}, nil
			}
			return procstat.Program{Name: "orch-hook"}, nil
		},
		started: tbl.startTime,
		peerPID: func(net.Conn) (int, error) { return 941, nil },
	}
	a := creds.identify(nil)
	if a.harness != "claude" || !a.harnessOrphan || a.harnessErr != nil {
		t.Fatalf("identify = %+v, want an orphaned harness", a)
	}
	if _, err := s.PinSession(callerCtx(a), pinReq("detached", dirs[1], "")); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PinSession from a detached harness = %v, want FailedPrecondition", err)
	}
	// A harness whose parent cannot be read is refused outright.
	creds.parent = func(pid int) (int, error) {
		if pid == 440 {
			return 0, errors.New("gone")
		}
		return 440, nil
	}
	if a := creds.identify(nil); a.harnessErr == nil {
		t.Errorf("identify with an unreadable harness parent = %+v", a)
	}
}
