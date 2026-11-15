package daemon

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
)

// Every RPC learns whom it serves (SP-011, INV-02, INV-05): the operator, a
// queued task, an interactive session by its pin, or no session. Resolution
// uses only what orchd read from the kernel when it accepted the connection,
// never a request field such as cwd, so an agent that runs
// `cd elsewhere && orch search` still resolves to its own session. The result
// decides which spaces the caller's memory reads see (readscope.go, SP-025);
// see docs/internals/daemon.md "Caller sessions".

// callerKind says who an RPC's caller is.
type callerKind string

const (
	// callerNone is a caller orchd cannot tie to the operator, a task or a
	// pinned session. Once spaces exist it reads only public content.
	callerNone     callerKind = "none"
	callerOperator callerKind = "operator"
	callerTask     callerKind = "task"
	callerSession  callerKind = "session"
)

// resolvedCaller is who an RPC's caller is.
type resolvedCaller struct {
	kind callerKind
	// pin is the session's pin when kind is callerSession.
	pin sessionPin
	// task is the queued task's run when kind is callerTask and orchd found
	// it from the caller's process tree; its TaskID is "" otherwise.
	task agentrun.LiveRun
	// why says why kind is callerNone, for logs.
	why string
	// harness and harnessProc are the nearest harness process above a
	// callerNone caller when it holds no pin, so the command log can keep
	// its events in that process's chain (D-36); empty otherwise.
	harness     string
	harnessProc procstat.Identity
	// operatorBy is the evidence that a callerOperator caller is the user
	// (operator_evidence.go), "" when there is none (D-71).
	operatorBy string
	// evidenceNeeded reports that a spaces policy was applied when the call
	// was resolved, so a callerOperator caller without operatorBy is not
	// the operator (D-71).
	evidenceNeeded bool
}

// connCaller caches what a connection resolves to. Only results that cannot
// change for the connection's life are kept: its task, and a pin held by the
// caller's own nearest harness (a pin's space never changes; a resume can
// move it to another process, so readers of per-session data check that the
// pin still names the caller's process). A pin
// found on an outer harness is not kept, nor is a miss: the nearest harness's
// SessionStart hook may pin it after the connection opened (an MCP server
// starts with the session).
type connCaller struct {
	mu   sync.Mutex
	pin  *sessionPin
	task *agentrun.LiveRun
}

// callerResolver resolves callers. A nil resolver, or one without pins,
// resolves no caller to a session.
type callerResolver struct {
	pins *pinRegistry
	// taskOf returns the queued task whose agent CLI is process proc; nil
	// when no tasks run here.
	taskOf func(proc procstat.Identity) (agentrun.LiveRun, bool)
	// needsEvidence reports whether a spaces policy is applied (or does not
	// verify), so the operator must be Orch.app (D-71); nil never needs it.
	needsEvidence func() bool
}

type resolvedKey struct{}

// callerOf returns the caller resolved for ctx. A context the resolver never
// saw has no session.
func callerOf(ctx context.Context) resolvedCaller {
	if c, ok := ctx.Value(resolvedKey{}).(resolvedCaller); ok {
		return c
	}
	return resolvedCaller{kind: callerNone, why: "the call was not resolved"}
}

func none(why string) resolvedCaller { return resolvedCaller{kind: callerNone, why: why} }

// resolve decides who ctx's caller is.
func (r *callerResolver) resolve(ctx context.Context) resolvedCaller {
	p, ok := peer.FromContext(ctx)
	var a callerAuth
	if ok {
		a, ok = p.AuthInfo.(callerAuth)
	}
	switch {
	case !ok:
		return none("unidentified process")
	case errors.Is(a.err, errPeerUnsupported):
		return none("socket peers cannot be read on this platform")
	case a.err != nil:
		return none("the caller's ancestry could not be read")
	case a.spawned || reportsTask(ctx):
		// Reason: the reported task ID only says the caller is a task, which
		// narrows it; its record comes from the process tree alone.
		return resolvedCaller{kind: callerTask, task: r.task(a)}
	case a.harnessErr != nil:
		return none("the caller's ancestry could not be read")
	case a.harness == "":
		c := resolvedCaller{kind: callerOperator, evidenceNeeded: r.needEvidence()}
		if c.evidenceNeeded && a.app.isApp() {
			c.operatorBy = operatorApp
		}
		return c
	}
	if r == nil || r.pins == nil {
		return none("session pins are unavailable")
	}
	pin, ok, err := r.pin(ctx, a)
	switch {
	case err != nil:
		log.Printf("orchd: resolving process %d to its session: %v", a.pid, err)
		return none("the session pins could not be read")
	case !ok:
		c := none("no harness above the caller is pinned")
		c.harness, c.harnessProc = a.harness, a.harnessProc
		return c
	}
	return resolvedCaller{kind: callerSession, pin: pin}
}

// reportsTask reports whether the client says it runs in a queued task.
func reportsTask(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	ids := md.Get(orchdclient.TaskMetadataKey)
	return len(ids) > 0 && ids[0] != ""
}

// task returns the run of the task a's process belongs to. Only orchd's own
// child on the caller's path, matched by PID and start time, can be a task's
// agent CLI.
func (r *callerResolver) task(a callerAuth) agentrun.LiveRun {
	if t := a.conn.cachedTask(); t != nil {
		return *t
	}
	if r == nil || r.taskOf == nil || !a.spawned {
		return agentrun.LiveRun{}
	}
	run, ok := r.taskOf(a.child)
	if !ok {
		return agentrun.LiveRun{}
	}
	return *a.conn.keepTask(run)
}

// pin returns the pin of the nearest harness at or above a's process that
// holds one, matching each by PID and the start time read at accept, so a
// reused PID never resolves to an earlier process's pin.
func (r *callerResolver) pin(ctx context.Context, a callerAuth) (sessionPin, bool, error) {
	if p := a.conn.cachedPin(); p != nil {
		return *p, true, nil
	}
	for i, h := range append([]procstat.Identity{a.harnessProc}, a.outerHarnesses...) {
		if h.PID <= 0 || h.Started.IsZero() {
			continue
		}
		p, ok, err := r.pins.currentOf(ctx, h)
		switch {
		case err != nil:
			return sessionPin{}, false, err
		case !ok:
			continue
		case i == 0:
			return *a.conn.keepPin(p), true, nil
		}
		return p, true, nil
	}
	return sessionPin{}, false, nil
}

// cachedPin, keepPin, cachedTask and keepTask read and fill c's cache; the
// lookups run outside its lock, and the first result kept wins. A nil c
// keeps nothing.
func (c *connCaller) cachedPin() *sessionPin {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pin
}

func (c *connCaller) keepPin(p sessionPin) *sessionPin {
	if c == nil {
		return &p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pin == nil {
		c.pin = &p
	}
	return c.pin
}

func (c *connCaller) cachedTask() *agentrun.LiveRun {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.task
}

func (c *connCaller) keepTask(run agentrun.LiveRun) *agentrun.LiveRun {
	if c == nil {
		return &run
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.task == nil {
		c.task = &run
	}
	return c.task
}

// unary attaches the resolved caller to each unary call's context.
func (r *callerResolver) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	c := r.resolve(ctx)
	if err := refuseUnproven(c, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(context.WithValue(ctx, resolvedKey{}, c), req)
}

// stream attaches the resolved caller to each stream's context.
func (r *callerResolver) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	c := r.resolve(ctx)
	if err := refuseUnproven(c, info.FullMethod); err != nil {
		return err
	}
	return handler(srv, resolvedStream{ServerStream: ss, ctx: context.WithValue(ctx, resolvedKey{}, c)})
}

// refuseUnproven refuses method to a caller that only its process tree
// shows to be the operator, while a policy is applied (D-71): such a caller
// (a terminal, or a process an agent detached) gets a queued task's limits,
// so every RPC in operatorActions is refused to it, not only those that
// check refuseNonOperator. Orch.app, sessions and, with no policy, the
// operator are not affected.
func refuseUnproven(c resolvedCaller, method string) error {
	if c.kind != callerOperator || !c.evidenceNeeded || c.operatorBy != "" || taskCallable[method] {
		return nil
	}
	action, ok := operatorActions[method]
	if !ok {
		action = "call " + method
	}
	if unprovenLogs.allow(time.Now()) {
		log.Printf("orchd: refused %s from a caller that is not Orch.app while a spaces policy is applied", rpcName(method))
	}
	return grpcstatus.Errorf(codes.PermissionDenied,
		"only Orch.app can %s while a spaces policy is applied (D-71); a terminal is not the operator then", action)
}

// resolvedStream is a server stream carrying the resolved caller.
type resolvedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s resolvedStream) Context() context.Context { return s.ctx }

// needEvidence reports whether the operator must show Orch.app's evidence
// now. A nil resolver needs none.
func (r *callerResolver) needEvidence() bool {
	return r != nil && r.needsEvidence != nil && r.needsEvidence()
}

// unprovenLogs limits refuseUnproven's log line.
var unprovenLogs = &logLimit{every: time.Second}
