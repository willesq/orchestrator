package daemon

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// resolveTree is a fake process tree for resolution tests. A terminal (30)
// runs claude 400 (pinned in dirs[0] by the test) and codex 410. Under claude
// a shell (902) runs an orch CLI (950) and a nested claude 420; under that
// nested claude another CLI (960) runs. Claude 401 is unrelated and unpinned,
// with its own CLI (970).
type resolveTree struct {
	parents map[int]int
	progs   map[int]procstat.Program
	tbl     procTable
}

func newResolveTree(dirs [2]string) *resolveTree {
	inner := claudeStart.Add(30 * time.Second)
	return &resolveTree{
		parents: map[int]int{950: 902, 902: 400, 400: 30, 30: 1, 410: 30, 951: 410,
			420: 902, 960: 420, 401: 30, 970: 401},
		progs: map[int]procstat.Program{
			950: {Name: "orch"}, 902: {Name: "zsh"}, 400: {Name: "claude"}, 30: {Name: "Terminal"},
			410: {Name: "codex"}, 951: {Name: "orch"}, 420: {Name: "claude"}, 960: {Name: "orch"},
			401: {Name: "claude"}, 970: {Name: "orch"},
		},
		tbl: procTable{
			started: map[int]time.Time{400: claudeStart, 410: codexStart, 420: inner, 401: inner},
			cwd:     map[int]string{400: dirs[0], 410: dirs[1], 420: dirs[1], 401: dirs[1], 950: dirs[1], 960: dirs[1]},
		},
	}
}

func (tr *resolveTree) creds(peerPID int) peerCredentials {
	return peerCredentials{
		self: 1000,
		parent: func(pid int) (int, error) {
			if pp, ok := tr.parents[pid]; ok {
				return pp, nil
			}
			return 0, errors.New("no such process")
		},
		program: func(pid int) (procstat.Program, error) {
			if p, ok := tr.progs[pid]; ok {
				return p, nil
			}
			return procstat.Program{}, errors.New("no such process")
		},
		started: tr.tbl.startTime,
		peerPID: func(net.Conn) (int, error) { return peerPID, nil },
	}
}

// connOf identifies peer pid as orchd would when accepting its connection.
func (tr *resolveTree) connOf(pid int) context.Context {
	return callerCtx(tr.creds(pid).identify(nil))
}

// pinnedResolver pins claude 400 in dirs[0] (sess-a) and codex 410 in dirs[1]
// (sess-c), and returns a resolver over those pins.
func pinnedResolver(t *testing.T) (*callerResolver, *resolveTree, [2]string) {
	t.Helper()
	s, dirs := pinsServer(t)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinSession(inCodex, pinReq("sess-c", dirs[1], "")); err != nil {
		t.Fatal(err)
	}
	return &callerResolver{pins: s.pins}, newResolveTree(dirs), dirs
}

// TestResolveCLIToAncestorPin is SP-011.1.a1: a CLI whose own cwd is in
// another folder resolves to its ancestor harness's pin.
func TestResolveCLIToAncestorPin(t *testing.T) {
	r, tr, dirs := pinnedResolver(t)
	// The CLI (950) runs in dirs[1], where codex's session is pinned.
	if c, _ := tr.tbl.workdir(950); c != dirs[1] {
		t.Fatalf("fake CLI cwd = %q", c)
	}
	got := r.resolve(tr.connOf(950))
	if got.kind != callerSession || got.pin.SessionID != "sess-a" || got.pin.FirstCwd != dirs[0] {
		t.Fatalf("resolve(950) = %+v, want sess-a in %s", got, dirs[0])
	}
	// A claude the agent started in dirs[1] took no pin of its own: a CLI
	// under it resolves to the nearest pinned harness above, claude 400.
	got = r.resolve(tr.connOf(960))
	if got.kind != callerSession || got.pin.SessionID != "sess-a" {
		t.Fatalf("resolve(960) under an unpinned nested claude = %+v, want sess-a", got)
	}
	// After /clear the same process holds a second session with the same
	// space; resolution names the live one.
	if err := r.pins.insert(context.Background(), sessionPin{
		SessionID: "sess-a2", Harness: "claude", Process: procstat.Identity{PID: 400, Started: claudeStart},
		FirstCwd: dirs[0], PinnedAt: claudeStart, InheritedFrom: "sess-a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pins.end(context.Background(), "sess-a", procstat.Identity{PID: 400, Started: claudeStart}); err != nil {
		t.Fatal(err)
	}
	if got := r.resolve(tr.connOf(950)); got.pin.SessionID != "sess-a2" || got.pin.FirstCwd != dirs[0] {
		t.Fatalf("after /clear resolve(950) = %+v, want sess-a2", got)
	}
}

// TestResolveIgnoresReusedPID is SP-011.1.a2: a reused PID with a different
// start time does not resolve to the old pin.
func TestResolveIgnoresReusedPID(t *testing.T) {
	r, tr, _ := pinnedResolver(t)
	// claude 400 exited and its PID now names a later claude.
	tr.tbl.started[400] = claudeStart.Add(time.Hour)
	got := r.resolve(tr.connOf(950))
	if got.kind != callerNone || got.pin.SessionID != "" {
		t.Fatalf("resolve under a reused PID = %+v, want no session", got)
	}
	// The same through a hand-built identity.
	reused := callerCtx(callerAuth{pid: 950, harness: "claude",
		harnessProc: procstat.Identity{PID: 400, Started: claudeStart.Add(-time.Minute)}})
	if got := r.resolve(reused); got.kind != callerNone {
		t.Fatalf("resolve(reused) = %+v, want no session", got)
	}
}

// TestResolveNoSession is SP-011.1.a3: broken lineage, an unknown harness
// and an unsupported platform each resolve to no session.
func TestResolveNoSession(t *testing.T) {
	t.Run("broken lineage", func(t *testing.T) {
		r, tr, _ := pinnedResolver(t)
		// The shell between the CLI and claude exited mid-walk.
		delete(tr.parents, 902)
		a := tr.creds(950).identify(nil)
		if a.err == nil && a.harnessErr == nil {
			t.Fatalf("identify over a broken walk = %+v", a)
		}
		if got := r.resolve(callerCtx(a)); got.kind != callerNone {
			t.Fatalf("resolve = %+v, want no session", got)
		}
		// Only the harness walk failed.
		if got := r.resolve(callerCtx(callerAuth{pid: 950, harnessErr: errors.New("gone")})); got.kind != callerNone {
			t.Fatalf("resolve with a failed harness walk = %+v", got)
		}
	})
	t.Run("unknown harness", func(t *testing.T) {
		r, tr, _ := pinnedResolver(t)
		// claude 401 never pinned a session.
		if got := r.resolve(tr.connOf(970)); got.kind != callerNone {
			t.Fatalf("resolve under unpinned claude 401 = %+v, want no session", got)
		}
	})
	t.Run("unsupported platform", func(t *testing.T) {
		r, tr, _ := pinnedResolver(t)
		// No peer lookup.
		if got := r.resolve(callerCtx(callerAuth{err: errPeerUnsupported})); got.kind != callerNone {
			t.Fatalf("resolve without peer lookup = %+v", got)
		}
		// Peer lookup but no start times (procstat.ErrUnsupported): the
		// harness has no identity to match.
		creds := tr.creds(950)
		creds.started = nil
		if got := r.resolve(callerCtx(creds.identify(nil))); got.kind != callerNone {
			t.Fatalf("resolve without start times = %+v", got)
		}
	})
}

// TestResolveIgnoresRequestCwd is SP-011.1.a4: a request's cwd plays no part.
// The CLI under claude 400 names dirs[1], where codex's session is pinned,
// and still resolves to its own session, through both interceptors.
func TestResolveIgnoresRequestCwd(t *testing.T) {
	r, tr, dirs := pinnedResolver(t)
	ctx := tr.connOf(950)
	var seen resolvedCaller
	_, err := r.unary(ctx, &orchdv1.DispatchRequest{Body: "x", Cwd: dirs[1]}, &grpc.UnaryServerInfo{},
		func(ctx context.Context, _ any) (any, error) { seen = callerOf(ctx); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if seen.kind != callerSession || seen.pin.SessionID != "sess-a" || seen.pin.FirstCwd != dirs[0] {
		t.Fatalf("unary resolved %+v, want sess-a whatever the request's cwd", seen)
	}
	seen = resolvedCaller{}
	err = r.stream(nil, fakeStream{ctx: metadata.NewIncomingContext(ctx, metadata.Pairs("cwd", dirs[1]))}, &grpc.StreamServerInfo{},
		func(_ any, ss grpc.ServerStream) error { seen = callerOf(ss.Context()); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if seen.kind != callerSession || seen.pin.SessionID != "sess-a" {
		t.Fatalf("stream resolved %+v, want sess-a", seen)
	}
}

func TestResolveOperatorAndTasks(t *testing.T) {
	runs := map[int]agentrun.LiveRun{77: {TaskID: "task-1", Dir: "/w"}}
	lookups := 0
	r := &callerResolver{taskOf: func(pid int) (agentrun.LiveRun, bool) {
		lookups++
		run, ok := runs[pid]
		return run, ok
	}}
	if got := r.resolve(callerCtx(callerAuth{pid: 42})); got.kind != callerOperator {
		t.Errorf("operator resolved to %+v", got)
	}
	// A task's process: orchd's child on its path is the task's agent CLI.
	conn := &connCaller{}
	task := callerCtx(callerAuth{pid: 79, spawned: true, child: 77, harness: "claude", conn: conn})
	if got := r.resolve(task); got.kind != callerTask || got.task.TaskID != "task-1" {
		t.Errorf("task resolved to %+v", got)
	}
	// Cached for the connection, even after the run ends.
	delete(runs, 77)
	if got := r.resolve(task); got.task.TaskID != "task-1" || lookups != 1 {
		t.Errorf("cached task = %+v after %d lookups", got, lookups)
	}
	// A spawned process with no run (a verification run) is a task without a
	// record.
	other := callerCtx(callerAuth{pid: 80, spawned: true, child: 78, conn: &connCaller{}})
	if got := r.resolve(other); got.kind != callerTask || got.task.TaskID != "" {
		t.Errorf("spawned process without a run = %+v", got)
	}
	// A client that reports a task ID is a task, but the ID names no record:
	// only the process tree does.
	runs[77] = agentrun.LiveRun{TaskID: "task-1"}
	claimed := metadata.NewIncomingContext(callerCtx(callerAuth{pid: 42}), metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	if got := r.resolve(claimed); got.kind != callerTask || got.task.TaskID != "" {
		t.Errorf("reported task = %+v, want a task without a record", got)
	}
	empty := metadata.NewIncomingContext(callerCtx(callerAuth{pid: 42}), metadata.Pairs(orchdclient.TaskMetadataKey, ""))
	if got := r.resolve(empty); got.kind != callerOperator {
		t.Errorf("an empty task ID = %+v, want the operator", got)
	}
	// Without a runner, a task has no record.
	fresh := callerCtx(callerAuth{pid: 79, spawned: true, child: 77})
	if got := (&callerResolver{}).resolve(fresh); got.kind != callerTask || got.task.TaskID != "" {
		t.Errorf("task without a runner = %+v", got)
	}
}

func TestResolveFailsClosed(t *testing.T) {
	r, tr, _ := pinnedResolver(t)
	for name, ctx := range map[string]context.Context{
		"no peer":         context.Background(),
		"foreign auth":    peer.NewContext(context.Background(), &peer.Peer{AuthInfo: insecureAuth{}}),
		"unknown process": callerCtx(callerAuth{err: errors.New("no peer")}),
	} {
		if got := r.resolve(ctx); got.kind != callerNone || got.why == "" {
			t.Errorf("%s resolved to %+v", name, got)
		}
	}
	if got := callerOf(context.Background()); got.kind != callerNone {
		t.Errorf("an unresolved call = %+v", got)
	}
	var nilResolver *callerResolver
	if got := nilResolver.resolve(tr.connOf(950)); got.kind != callerNone {
		t.Errorf("nil resolver = %+v", got)
	}
	// A store that cannot be read gives no session rather than an error.
	if err := r.pins.db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := r.resolve(tr.connOf(950)); got.kind != callerNone {
		t.Errorf("unreadable pins = %+v", got)
	}
}

// TestResolveCachesPerConnection: a connection keeps its pin once found, and
// looks again while its harness has none (its SessionStart may come later).
func TestResolveCachesPerConnection(t *testing.T) {
	s, dirs := pinsServer(t)
	r, tr := &callerResolver{pins: s.pins}, newResolveTree(dirs)
	conn := tr.connOf(950)
	if got := r.resolve(conn); got.kind != callerNone {
		t.Fatalf("before SessionStart = %+v", got)
	}
	if _, err := s.PinSession(inClaude, pinReq("sess-late", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if got := r.resolve(conn); got.pin.SessionID != "sess-late" {
		t.Fatalf("after SessionStart = %+v", got)
	}
	if _, err := s.pins.db.Exec(`DELETE FROM session_pins`); err != nil {
		t.Fatal(err)
	}
	if got := r.resolve(conn); got.pin.SessionID != "sess-late" {
		t.Fatalf("cached = %+v", got)
	}
	if got := r.resolve(tr.connOf(950)); got.kind != callerNone {
		t.Fatalf("a new connection = %+v, want a fresh lookup", got)
	}
}
