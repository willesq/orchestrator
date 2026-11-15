package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// unlockTree extends operatorTree for SP-077. orchd is 1000.
//
//	zsh 31 → orch 32           'orch spaces unlock' (the requester)
//	orch 32 → zsh 33 → orch 34 a command in the unlocked shell
//	zsh 31 → orch 36           another command in the same terminal
//	zsh 33 → claude 38 → sh 39 an agent started in the unlocked shell
//	orchd 1000 → claude 60 → orch 61   a task orchd runs
func unlockTree() operatorTree {
	tr := newOperatorTree()
	for pid, pp := range map[int]int{33: 32, 34: 33, 36: 31, 38: 33, 39: 38, 1000: 1, 60: 1000, 61: 60} {
		tr.parents[pid] = pp
	}
	for pid, p := range map[int]procstat.Program{
		32: {Name: "orch", Args: []string{"orch", "spaces", "unlock", "--for", "15m"}}, 33: {Name: "zsh"}, 34: {Name: "orch"},
		36: {Name: "orch"}, 38: {Name: "claude"}, 39: {Name: "sh"}, 1000: {Name: "orchd"}, 60: {Name: "claude"}, 61: {Name: "orch"},
	} {
		tr.progs[pid] = p
	}
	return tr
}

// unlockEnv is a server with a policy applied, the unlock registry on a
// fake process table and clock, and a resolver like orchd's.
type unlockEnv struct {
	readEnv
	tr  operatorTree
	reg *unlockRegistry
	r   *callerResolver

	mu      sync.Mutex
	clock   time.Time
	started map[int]time.Time
	// replaced are PIDs whose connection's opener has exited (D-44).
	replaced map[int]bool
}

func newUnlockEnv(t *testing.T) *unlockEnv {
	t.Helper()
	e := &unlockEnv{readEnv: newReadEnv(t), tr: unlockTree(), clock: time.Unix(1_800_000_000, 0), started: map[int]time.Time{},
		replaced: map[int]bool{}}
	for pid := range e.tr.parents {
		e.started[pid] = time.Unix(int64(1_700_000_000+pid), 0)
	}
	e.reg = &unlockRegistry{
		byID: map[string]*unlock{},
		now:  func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock },
		parent: func(p int) (int, error) {
			if pp, ok := e.tr.parents[p]; ok {
				return pp, nil
			}
			return 0, errors.New("no such process")
		},
		started: e.startTime,
		program: func(p int) (procstat.Program, error) {
			if pr, ok := e.tr.progs[p]; ok {
				return pr, nil
			}
			return procstat.Program{}, errors.New("no such process")
		},
		tty: func(p int) (string, error) {
			if p == 40 {
				return "", errors.New("unreadable")
			}
			return "ttys0" + string(rune('0'+p%10)), nil
		},
		samePeer: func(c net.Conn, p int) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if _, ok := c.(signedConn); !ok || e.replaced[p] {
				return errPeerReplaced
			}
			return nil
		},
	}
	e.s.unlocks = e.reg
	e.r = &callerResolver{pins: e.s.pins, needsEvidence: policyApplied(e.s.spaces), unlocks: e.reg}
	return e
}

func (e *unlockEnv) startTime(p int) (time.Time, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.started[p]; ok {
		return s, nil
	}
	return time.Time{}, errors.New("no such process")
}

func (e *unlockEnv) setStarted(pid int, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started[pid] = at
}

func (e *unlockEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

var (
	genuineApp = appSig("/Applications/Orch.app", codeRuntime, genuineCDHash)
	unsigned   = signedConn{err: errors.New("not signed")}
)

// authOn is what orchd learns when process pid connects over conn.
func (e *unlockEnv) authOn(pid int, conn net.Conn) callerAuth {
	creds := e.tr.creds(pid, fakeVerifier())
	creds.started = e.startTime
	return creds.identify(conn)
}

func (e *unlockEnv) auth(pid int) callerAuth { return e.authOn(pid, unsigned) }

// call makes method from process pid over conn through orchd's
// interceptors, in their order: the queued-task guard, then the resolver.
func (e *unlockEnv) call(pid int, conn net.Conn, method string, fn func(context.Context) (any, error)) (any, error) {
	ctx := callerCtx(e.authOn(pid, conn))
	info := &grpc.UnaryServerInfo{FullMethod: method}
	return guardQueuedCallers(ctx, nil, info, func(ctx context.Context, _ any) (any, error) {
		return e.r.unary(ctx, nil, info, func(ctx context.Context, _ any) (any, error) { return fn(ctx) })
	})
}

func (e *unlockEnv) resolved(pid int, conn net.Conn) resolvedCaller {
	return e.r.resolve(callerCtx(e.authOn(pid, conn)))
}

func (e *unlockEnv) list(t *testing.T) []*orchdv1.OperatorUnlock {
	t.Helper()
	out, err := e.call(50, genuineApp, orchdv1.Orchd_ListUnlocks_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.ListUnlocks(ctx, &orchdv1.ListUnlocksRequest{})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*orchdv1.ListUnlocksResponse).GetUnlocks()
}

// listed is the unlock Orch.app lists for process pid in state.
func (e *unlockEnv) listed(t *testing.T, pid int, state string) *orchdv1.OperatorUnlock {
	t.Helper()
	for _, u := range e.list(t) {
		if int(u.GetPid()) == pid && u.GetState() == state {
			return u
		}
	}
	t.Fatalf("Orch.app lists no %s unlock of process %d", state, pid)
	return nil
}

func (e *unlockEnv) confirm(pid int, conn net.Conn, id string, allow bool) error {
	_, err := e.call(pid, conn, orchdv1.Orchd_ConfirmUnlock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.ConfirmUnlock(ctx, &orchdv1.ConfirmUnlockRequest{Id: id, Allow: allow})
	})
	return err
}

func (e *unlockEnv) prove(pid int, id, code string) (*orchdv1.OperatorUnlock, error) {
	out, err := e.call(pid, unsigned, orchdv1.Orchd_ProveUnlock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.ProveUnlock(ctx, &orchdv1.ProveUnlockRequest{Id: id, Code: code})
	})
	if err != nil {
		return nil, err
	}
	return out.(*orchdv1.ProveUnlockResponse).GetUnlock(), nil
}

func (e *unlockEnv) lock(pid int, conn net.Conn, id string) (int32, error) {
	out, err := e.call(pid, conn, orchdv1.Orchd_Lock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.Lock(ctx, &orchdv1.LockRequest{Id: id})
	})
	if err != nil {
		return 0, err
	}
	return out.(*orchdv1.LockResponse).GetEnded(), nil
}

// requested is an unlock request waiting in RequestUnlock.
type requested struct {
	done chan struct{}
	out  *orchdv1.RequestUnlockResponse
	err  error
}

// request starts RequestUnlock from pid and waits until Orch.app lists it
// (or it is refused at once).
func (e *unlockEnv) request(t *testing.T, ctx context.Context, pid int, secs int64) *requested {
	t.Helper()
	before := len(e.list(t))
	r := &requested{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		out, err := e.call(pid, unsigned, orchdv1.Orchd_RequestUnlock_FullMethodName, ctxFn(func(ctx context.Context) (any, error) {
			return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: secs})
		}).withCtx(ctx))
		r.err = err
		if err == nil {
			r.out = out.(*orchdv1.RequestUnlockResponse)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-r.done:
			return r
		default:
		}
		if len(e.list(t)) > before {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the request from process %d never reached Orch.app", pid)
	return nil
}

func (r *requested) wait(t *testing.T) (*orchdv1.RequestUnlockResponse, error) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestUnlock did not return")
	}
	return r.out, r.err
}

// grant runs the whole unlock for pid: request, Orch.app's confirmation,
// and the code typed in the terminal.
func (e *unlockEnv) grant(t *testing.T, pid int, secs int64) *orchdv1.OperatorUnlock {
	t.Helper()
	r := e.request(t, context.Background(), pid, secs)
	u := e.listed(t, pid, unlockPending)
	if err := e.confirm(50, genuineApp, u.GetId(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.wait(t); err != nil {
		t.Fatal(err)
	}
	got, err := e.prove(pid, u.GetId(), e.listed(t, pid, unlockConfirmed).GetCode())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// withCtx runs fn with ctx's cancellation instead of the context the
// interceptors pass, so a test can withdraw a request.
type ctxFn func(context.Context) (any, error)

func (fn ctxFn) withCtx(outer context.Context) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		merged, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(outer, cancel)
		defer stop()
		return fn(merged)
	}
}

// operatorScope reports whether pid is the operator: it reads every space
// and may diff the policy, and its evidence is the unlock.
func (e *unlockEnv) operatorScope(t *testing.T, pid int) bool {
	t.Helper()
	c := e.resolved(pid, unsigned)
	_, err := e.call(pid, unsigned, orchdv1.Orchd_DiffSpaces_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{})
	})
	got := e.readAll(t, as(c), memoText("globex", 3), memoURI("globex", 3))
	reads := slices.Contains(got, memoURI("globex", 3)) && slices.Contains(got, memoURI("acme", 3))
	if (err == nil) != reads || (c.operatorBy == operatorUnlock) != reads {
		t.Fatalf("process %d: DiffSpaces %v, reads every space %v, evidence %q: they disagree", pid, err, reads, c.operatorBy)
	}
	if !reads {
		if l := spaceLeaks([]string{"oss"}, got...); len(l) > 0 {
			t.Fatalf("process %d read beyond public: %q", pid, l[:min(3, len(l))])
		}
	}
	return reads
}

func unlockCodeForm(c string) bool {
	return len(c) == 7 && c[3] == '-' && !strings.ContainsFunc(c[:3]+c[4:], func(r rune) bool { return r < '0' || r > '9' })
}

// wrongCode is a well-formed code other than c.
func wrongCode(c string) string {
	if c == "000-000" {
		return "000-001"
	}
	return "000-000"
}

// TestUnlockGrantsScopeOnlyOnceConfirmed is SP-077.1.a1: 'orch spaces
// unlock' leaves a pending request that grants nothing; Orch.app's
// confirmation grants nothing either until the requester sends the code
// Orch.app shows; then the requesting process and its descendants are the
// operator (an agent harness among them excepted, see
// TestUnlockFollowsTheConnectionsProcess), nobody else, until the duration
// ends, a lock ends it, or the requester exits; an unlock lasts at most 60
// minutes.
func TestUnlockGrantsScopeOnlyOnceConfirmed(t *testing.T) {
	e := newUnlockEnv(t)
	req := e.request(t, context.Background(), 32, 15*60)

	// Pending: nothing granted.
	pending := e.list(t)
	if len(pending) != 1 || pending[0].GetState() != unlockPending || pending[0].GetPid() != 32 || pending[0].GetForSeconds() != 900 {
		t.Fatalf("listed %v, want one pending 15m request from process 32", pending)
	}
	if got := pending[0].GetCommand(); got != "orch spaces unlock --for 15m" {
		t.Fatalf("command shown %q", got)
	}
	if got := pending[0].GetAncestry(); len(got) != 2 || !strings.HasPrefix(got[0], "zsh (31)") || !strings.HasPrefix(got[1], "Terminal (30)") {
		t.Fatalf("ancestry shown %q", got)
	}
	if pending[0].GetCode() != "" {
		t.Fatal("a pending request has a code")
	}
	if got := pending[0].GetTty(); got != "ttys02" {
		t.Fatalf("terminal shown %q", got)
	}
	for _, pid := range []int{32, 34} {
		if e.operatorScope(t, pid) {
			t.Fatalf("process %d is the operator before Orch.app confirmed", pid)
		}
	}

	// Confirmed: the app shows a code; the requester's answer has none, and
	// nothing is granted yet.
	id := pending[0].GetId()
	if err := e.confirm(50, genuineApp, id, true); err != nil {
		t.Fatal(err)
	}
	out, err := req.wait(t)
	if err != nil || out.GetUnlock().GetState() != unlockConfirmed || out.GetUnlock().GetCode() != "" {
		t.Fatalf("RequestUnlock = %v, %v; want confirmed, without the code", out, err)
	}
	code := e.listed(t, 32, unlockConfirmed).GetCode()
	if !unlockCodeForm(code) {
		t.Fatalf("Orch.app shows code %q", code)
	}
	if e.operatorScope(t, 32) || e.operatorScope(t, 34) {
		t.Fatal("a confirmed unlock granted scope before its code was sent")
	}
	// Only the requester proves it, with the right code.
	if _, err := e.prove(34, id, code); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("the code from a descendant = %v, want refused", err)
	}
	if _, err := e.prove(32, id, wrongCode(code)); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("a wrong code = %v", err)
	}
	if _, err := e.prove(32, "other", code); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("another id = %v", err)
	}
	got, err := e.prove(32, id, code)
	if err != nil || got.GetState() != unlockActive || got.GetCode() != "" || got.GetExpiresAt() != e.clock.Add(15*time.Minute).Unix() {
		t.Fatalf("ProveUnlock = %v, %v; want active for 15m", got, err)
	}
	if _, err := e.prove(32, id, code); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("proving twice = %v", err)
	}

	// The requester and its descendants are the operator; its siblings,
	// parents and other terminals are not. An agent started under it is not
	// either, and ends it (TestUnlockFollowsTheConnectionsProcess).
	for pid, want := range map[int]bool{32: true, 34: true, 33: true, 31: false, 36: false, 37: false, 40: false} {
		if got := e.operatorScope(t, pid); got != want {
			t.Errorf("process %d operator %v, want %v", pid, got, want)
		}
	}

	// It ends with its duration.
	e.advance(15*time.Minute - time.Second)
	if !e.operatorScope(t, 34) {
		t.Fatal("the unlock ended early")
	}
	e.advance(time.Second)
	if e.operatorScope(t, 34) || len(e.list(t)) != 0 {
		t.Fatal("the unlock outlived its duration")
	}

	// Or with 'orch spaces lock' from the unlocked shell, which ends only
	// what covers it.
	e.grant(t, 37, 60)
	e.grant(t, 32, 3600)
	if n, err := e.lock(36, unsigned, ""); err != nil || n != 0 {
		t.Fatalf("a sibling's lock ended %d, %v", n, err)
	}
	if n, err := e.lock(34, unsigned, ""); err != nil || n != 1 {
		t.Fatalf("lock from the unlocked shell ended %d, %v; want 1", n, err)
	}
	if e.operatorScope(t, 34) || !e.operatorScope(t, 37) {
		t.Fatal("lock did not end exactly the shell's unlock")
	}
	// Orch.app can lock any.
	if n, err := e.lock(50, genuineApp, ""); err != nil || n != 1 {
		t.Fatalf("Orch.app's lock ended %d, %v", n, err)
	}

	// It is the process that asked: once it exits, a later process with
	// its PID is not covered, and the unlock is gone.
	e.advance(time.Minute)
	e.grant(t, 32, 600)
	if !e.operatorScope(t, 34) {
		t.Fatal("unlock not in force")
	}
	before, _ := e.startTime(32)
	e.setStarted(32, before.Add(time.Second))
	if e.operatorScope(t, 34) || e.operatorScope(t, 32) {
		t.Fatal("a process reusing the requester's PID carries its unlock")
	}
	if got := e.list(t); len(got) != 0 {
		t.Fatalf("Orch.app still lists the exited process's unlock: %v", got)
	}

	// At most 60 minutes, at least a second.
	for _, secs := range []int64{0, -1, 3601} {
		_, err := e.call(36, unsigned, orchdv1.Orchd_RequestUnlock_FullMethodName, func(ctx context.Context) (any, error) {
			return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: secs})
		})
		if grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("an unlock for %ds = %v, want refused", secs, err)
		}
	}
}
