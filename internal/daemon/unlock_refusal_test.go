package daemon

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestUnlockRequestEndsUnconfirmed: a denied, lapsed, withdrawn or locked
// request grants nothing, nor does a confirmed one whose code never comes
// or comes wrong three times; requests are bounded.
func TestUnlockRequestEndsUnconfirmed(t *testing.T) {
	e := newUnlockEnv(t)
	check := func(name string, r *requested, want codes.Code) {
		t.Helper()
		if _, err := r.wait(t); grpcstatus.Code(err) != want {
			t.Errorf("%s: RequestUnlock = %v, want %s", name, err, want)
		}
		if e.operatorScope(t, 34) || len(e.list(t)) != 0 {
			t.Errorf("%s: something was granted or kept", name)
		}
	}
	r := e.request(t, context.Background(), 32, 60)
	if err := e.confirm(50, genuineApp, e.list(t)[0].GetId(), false); err != nil {
		t.Fatal(err)
	}
	check("denied", r, codes.PermissionDenied)

	r = e.request(t, context.Background(), 32, 60)
	e.advance(unlockPendingFor)
	e.list(t) // a read forgets what lapsed
	check("lapsed", r, codes.DeadlineExceeded)

	ctx, cancel := context.WithCancel(context.Background())
	r = e.request(t, ctx, 32, 60)
	cancel()
	check("withdrawn", r, codes.Canceled)

	r = e.request(t, context.Background(), 32, 60)
	if n, err := e.lock(50, genuineApp, e.list(t)[0].GetId()); err != nil || n != 1 {
		t.Fatalf("Orch.app locking a pending request ended %d, %v", n, err)
	}
	check("locked", r, codes.PermissionDenied)

	// Confirmed, then no code within the time, or three wrong ones.
	if err := e.confirm(50, genuineApp, "nope", true); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("confirming an unknown request = %v", err)
	}
	e.advance(time.Minute)
	confirmed := func() string {
		t.Helper()
		r := e.request(t, context.Background(), 32, 60)
		id := e.list(t)[0].GetId()
		if err := e.confirm(50, genuineApp, id, true); err != nil {
			t.Fatal(err)
		}
		if _, err := r.wait(t); err != nil {
			t.Fatal(err)
		}
		if err := e.confirm(50, genuineApp, id, true); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("confirming twice = %v", err)
		}
		return id
	}
	id := confirmed()
	e.advance(unlockPendingFor)
	if _, err := e.prove(32, id, "000-000"); grpcstatus.Code(err) != codes.NotFound || len(e.list(t)) != 0 {
		t.Fatalf("a code after the request lapsed = %v", err)
	}
	id = confirmed()
	code := e.listed(t, 32, unlockConfirmed).GetCode()
	for i, want := range []codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.PermissionDenied} {
		if _, err := e.prove(32, id, wrongCode(code)); grpcstatus.Code(err) != want {
			t.Fatalf("wrong code %d = %v, want %s", i+1, err, want)
		}
	}
	if _, err := e.prove(32, id, code); grpcstatus.Code(err) != codes.NotFound || e.operatorScope(t, 34) {
		t.Fatalf("the right code after three wrong ones = %v", err)
	}

	// One waiting request per process, a few in all, and a few a minute.
	e.advance(time.Minute)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	e.request(t, ctx, 32, 60)
	if _, err := e.reg.request(e.auth(32), time.Minute); err == nil {
		t.Error("a process waits for two unlocks")
	}
	for _, pid := range []int{36, 37, 40} {
		e.request(t, ctx, pid, 60)
	}
	if got := e.listed(t, 40, unlockPending).GetTty(); got != "(unknown)" {
		t.Errorf("an unreadable terminal is shown as %q", got)
	}
	if _, err := e.reg.request(e.auth(34), time.Minute); !errors.Is(err, errUnlockLimit) {
		t.Errorf("a fifth pending request = %v, want the limit", err)
	}
	e.lock(50, genuineApp, "")
	e.request(t, ctx, 32, 60)
	e.request(t, ctx, 36, 60)
	if _, err := e.reg.request(e.auth(37), time.Minute); !errors.Is(err, errUnlockLimit) {
		t.Errorf("a seventh request in a minute = %v, want the limit", err)
	}
	e.advance(time.Minute)
	if _, err := e.reg.request(e.auth(37), time.Minute); err != nil {
		t.Errorf("a request a minute later = %v", err)
	}
	if _, err := e.reg.request(callerAuth{pid: 999}, time.Minute); err == nil {
		t.Error("a process that cannot be identified asked for an unlock")
	}
	e.replaced[34] = true
	if _, err := e.reg.request(e.auth(34), time.Minute); err == nil {
		t.Error("a connection whose opener exited asked for an unlock")
	}
}

// TestUnlockRefusedToAgentsAndNonApp is SP-077.1.a2: an unlock request
// (or its code) from a caller descended from a harness or from orchd is
// refused, and so is a confirmation (or the list of requests) that does not
// come from Orch.app, including from an unlocked terminal.
func TestUnlockRefusedToAgentsAndNonApp(t *testing.T) {
	e := newUnlockEnv(t)
	for name, pid := range map[string]int{"an agent in a shell": 39, "an agent harness": 38, "a task orchd runs": 61} {
		_, err := e.call(pid, unsigned, orchdv1.Orchd_RequestUnlock_FullMethodName, func(ctx context.Context) (any, error) {
			return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: 60})
		})
		if grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("RequestUnlock from %s = %v, want refused", name, err)
		}
		if _, err := e.prove(pid, "x", "000-000"); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ProveUnlock from %s = %v, want refused", name, err)
		}
		if _, err := e.lock(pid, unsigned, ""); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("Lock from %s = %v, want refused", name, err)
		}
	}
	// A pinned session asks through the handler directly.
	if _, err := e.s.RequestUnlock(inSpace("acme"), &orchdv1.RequestUnlockRequest{ForSeconds: 60}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("RequestUnlock from a session = %v", err)
	}
	if _, err := e.s.Lock(inSpace("acme"), &orchdv1.LockRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("Lock from a session = %v", err)
	}
	// Orch.app asks for nothing.
	_, err := e.call(50, genuineApp, orchdv1.Orchd_RequestUnlock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: 60})
	})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("RequestUnlock from Orch.app = %v", err)
	}

	r := e.request(t, context.Background(), 32, 600)
	id := e.list(t)[0].GetId()
	for name, who := range map[string]struct {
		pid  int
		conn net.Conn
	}{
		"the requester":         {32, unsigned},
		"another terminal":      {36, unsigned},
		"a pty of its own":      {37, unsigned},
		"a detached process":    {40, unsigned},
		"an agent":              {39, unsigned},
		"a task":                {61, unsigned},
		"an unsigned Orch":      {50, unsigned},
		"an Orch.app elsewhere": {50, appSig("/tmp/x/Orch.app", codeRuntime, genuineCDHash)},
	} {
		if err := e.confirm(who.pid, who.conn, id, true); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ConfirmUnlock from %s = %v, want refused", name, err)
		}
		_, err := e.call(who.pid, who.conn, orchdv1.Orchd_ListUnlocks_FullMethodName, func(ctx context.Context) (any, error) {
			return e.s.ListUnlocks(ctx, &orchdv1.ListUnlocksRequest{})
		})
		if grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ListUnlocks from %s = %v, want refused", name, err)
		}
	}
	if e.operatorScope(t, 32) {
		t.Fatal("a refused confirmation granted the unlock")
	}
	if err := e.confirm(50, genuineApp, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prove(32, id, e.listed(t, 32, unlockConfirmed).GetCode()); err != nil {
		t.Fatal(err)
	}
	// The unlocked terminal is the operator, but not Orch.app: it cannot
	// list or confirm another unlock.
	r2 := e.request(t, context.Background(), 36, 60)
	id2 := e.listed(t, 36, unlockPending).GetId()
	if err := e.confirm(34, unsigned, id2, true); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("an unlocked terminal confirmed an unlock: %v", err)
	}
	if _, err := e.call(34, unsigned, orchdv1.Orchd_ListUnlocks_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.ListUnlocks(ctx, &orchdv1.ListUnlocksRequest{})
	}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("an unlocked terminal listed the unlocks (and their codes): %v", err)
	}
	if err := e.confirm(50, genuineApp, id2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.wait(t); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("denied request = %v", err)
	}
}

// TestUnlockConfirmationNeedsTheSignedApp is SP-077.1.a3: only the
// installed Orch.app, by its code signature, confirms an unlock (and sees
// its code); a signed copy elsewhere, a program at the app's path with
// another signature, the app without the hardened runtime, or one that
// cannot be checked is refused (RC-706). TestAppSignatureOfARealProcess
// checks the same recognition against real signatures.
func TestUnlockConfirmationNeedsTheSignedApp(t *testing.T) {
	e := newUnlockEnv(t)
	r := e.request(t, context.Background(), 32, 60)
	id := e.list(t)[0].GetId()
	for name, conn := range map[string]net.Conn{
		"a signed copy elsewhere":              appSig("/Volumes/copy/Orch.app", codeRuntime, genuineCDHash),
		"another signature at the app's path":  appSig("/Applications/Orch.app", codeRuntime, []byte{9, 9}),
		"the app without the hardened runtime": appSig("/Applications/Orch.app", 0, genuineCDHash),
		"a program with no code hash":          appSig("/Applications/Orch.app/Contents/MacOS/Orch", codeRuntime, nil),
		"a program that fails the requirement": signedConn{err: errors.New("requirement not met")},
	} {
		if err := e.confirm(50, conn, id, true); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ConfirmUnlock from %s = %v, want refused", name, err)
		}
		_, err := e.call(50, conn, orchdv1.Orchd_ListUnlocks_FullMethodName, func(ctx context.Context) (any, error) {
			return e.s.ListUnlocks(ctx, &orchdv1.ListUnlocksRequest{})
		})
		if grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ListUnlocks from %s = %v, want refused", name, err)
		}
	}
	if e.operatorScope(t, 34) {
		t.Fatal("an unlock was granted without Orch.app")
	}
	if err := e.confirm(50, appSig("/Applications/Orch.app/Contents/MacOS/Orch", codeRuntime, genuineCDHash), id, true); err != nil {
		t.Fatalf("the installed Orch.app could not confirm: %v", err)
	}
	if _, err := r.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prove(32, id, e.listed(t, 32, unlockConfirmed).GetCode()); err != nil || !e.operatorScope(t, 34) {
		t.Fatalf("confirmed unlock not in force: %v", err)
	}
}

// TestUnlockFollowsTheConnectionsProcess (D-44, RC-711): a connection whose
// opener exited is not covered when its PID is reused under an unlock; an
// agent harness starting under an unlocked shell ends the unlock, unless its
// connection outlived the harness; a requester that gave up as it was
// confirmed leaves nothing behind.
func TestUnlockFollowsTheConnectionsProcess(t *testing.T) {
	e := newUnlockEnv(t)
	e.grant(t, 32, 600)
	// A detached process (70, under launchd) opened a connection, handed
	// it on and exited; its PID now names a command in the unlocked shell.
	e.tr.parents[70], e.tr.progs[70] = 33, procstat.Program{Name: "orch"}
	e.setStarted(70, time.Unix(1_700_000_070, 0))
	if !e.operatorScope(t, 70) {
		t.Fatal("a command in the unlocked shell is not the operator")
	}
	e.mu.Lock()
	e.replaced[70] = true
	e.mu.Unlock()
	if e.operatorScope(t, 70) {
		t.Fatal("a connection whose opener exited inherits the unlock of the process reusing its PID")
	}
	if n, err := e.lock(70, unsigned, ""); err != nil || n != 0 {
		t.Fatalf("that connection ended %d unlocks, %v", n, err)
	}
	// A harness connection whose harness has exited ends nothing.
	stale := e.auth(39)
	e.setStarted(38, time.Unix(1, 0))
	e.reg.endUnder(stale)
	if !e.operatorScope(t, 34) {
		t.Fatal("a connection that outlived its harness ended the unlock")
	}
	e.setStarted(38, time.Unix(1_700_000_038, 0))
	e.reg.endUnder(callerAuth{pid: 39})
	if !e.operatorScope(t, 34) {
		t.Fatal("a caller with no harness identity ended the unlock")
	}
	// An agent started in the unlocked shell ends the unlock with its
	// first call.
	if c := e.resolved(39, unsigned); c.kind == callerOperator {
		t.Fatal("the agent resolved as the operator")
	}
	if e.operatorScope(t, 34) || len(e.list(t)) != 0 {
		t.Fatal("the unlock outlived an agent starting under it")
	}
	e.reg.endUnder(e.auth(39)) // nothing left: a no-op
	(*unlockRegistry)(nil).endUnder(e.auth(39))

	// Confirmed just as the requester gave up: nothing is left.
	u, err := e.reg.request(e.auth(36), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reg.confirm(u.id, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.reg.wait(ctx, u); grpcstatus.Code(err) != codes.Canceled {
		t.Fatalf("wait after giving up = %v", err)
	}
	if len(e.list(t)) != 0 {
		t.Fatal("a confirmed request outlived its requester giving up")
	}
}

// TestUnlockWithoutPolicy: with no policy a terminal is the operator
// already (INV-10), so there is nothing to unlock and Orch.app sees an
// empty list.
func TestUnlockWithoutPolicy(t *testing.T) {
	e := newUnlockEnv(t)
	e.r.needsEvidence = policyApplied(nil)
	_, err := e.call(32, unsigned, orchdv1.Orchd_RequestUnlock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: 60})
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("RequestUnlock with no policy = %v", err)
	}
	if got := e.list(t); len(got) != 0 {
		t.Fatalf("ListUnlocks with no policy = %v", got)
	}
	if n, err := e.lock(32, unsigned, ""); err != nil || n != 0 {
		t.Fatalf("Lock with no policy = %d, %v", n, err)
	}
	// Without a registry nothing is unlocked and nothing fails.
	e.s.unlocks, e.r.unlocks = nil, nil
	if (*unlockRegistry)(nil).covers(callerAuth{pid: 32}) {
		t.Fatal("a nil registry covers a process")
	}
	e.r.needsEvidence = policyApplied(e.s.spaces)
	if _, err := e.call(32, unsigned, orchdv1.Orchd_RequestUnlock_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: 60})
	}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("RequestUnlock without a registry = %v", err)
	}
	if _, err := e.prove(32, "x", "000-000"); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("ProveUnlock without a registry = %v", err)
	}
	if got := e.list(t); len(got) != 0 {
		t.Fatalf("ListUnlocks without a registry = %v", got)
	}
	if err := e.confirm(50, genuineApp, "x", true); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("ConfirmUnlock without a registry = %v", err)
	}
	if n, err := e.lock(32, unsigned, ""); err != nil || n != 0 {
		t.Fatalf("Lock without a registry = %d, %v", n, err)
	}
}

// TestUnlockDescriptionIsPrintable: what the app shows cannot hide text
// from the user or grow without bound, and codes have one form.
func TestUnlockDescriptionIsPrintable(t *testing.T) {
	if got := printable("orch\x1b[2J\u202espaces\nunlock x\u2066y\U000E0041z", 100); got != "orch?[2J?spaces?unlock?x?y?z" {
		t.Fatalf("printable = %q", got)
	}
	for in, want := range map[string]string{"orch": "orch", "": "''", "a b": "'a b'", "it's": `'it'\''s'`, "--for=15m": "--for=15m"} {
		if got := argQuote(in); got != want {
			t.Errorf("argQuote(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 300)
	if got := printable(long, 11); got != strings.Repeat("é", 5)+"…" {
		t.Fatalf("cut = %q", got)
	}
	r := newUnlockRegistry()
	if cmd, _, _ := r.describe(1 << 30); cmd != "(unknown)" {
		t.Fatalf("an unknown process is described as %q", cmd)
	}
	r.program = func(int) (procstat.Program, error) { return procstat.Program{Name: "zsh"}, nil }
	r.parent = func(p int) (int, error) { return p + 1, nil }
	cmd, exe, anc := r.describe(10)
	if cmd != "zsh" || exe != "" || len(anc) != maxUnlockAncestry {
		t.Fatalf("described %q %q %v", cmd, exe, anc)
	}
	r.program = func(int) (procstat.Program, error) {
		return procstat.Program{Name: "orch", Path: "/opt/orch/bin/orch", Args: []string{"orch", "spaces", "unlock", "a b"}}, nil
	}
	if cmd, exe, _ := r.describe(10); cmd != "orch spaces unlock 'a b'" || exe != "/opt/orch/bin/orch" {
		t.Fatalf("described %q %q", cmd, exe)
	}
	seen := map[string]bool{}
	for range 50 {
		c, err := unlockCode()
		if err != nil || !unlockCodeForm(c) {
			t.Fatalf("code %q, %v", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 45 {
		t.Fatalf("codes repeat: %d distinct of 50", len(seen))
	}
}
