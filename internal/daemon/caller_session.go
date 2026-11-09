package daemon

import (
	"context"
	"errors"
	"log"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
)

// Every RPC learns whom it serves (SP-011, INV-02, INV-05): the operator, a
// queued task, an interactive session by its pin, or no session. Resolution
// uses only what orchd read from the kernel when it accepted the connection,
// never a request field such as cwd, so an agent that runs
// `cd elsewhere && orch search` still resolves to its own session. Nothing
// grants access by the result yet (RC-01); see docs/internals/daemon.md
// "Caller sessions".

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
}

// connCaller caches what a connection resolves to. Only results that cannot
// change for the connection's life are kept: its task, and a pin held by the
// caller's own nearest harness (a pin never changes but for ended_at). A pin
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
		return resolvedCaller{kind: callerOperator}
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
		return none("no harness above the caller is pinned")
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
func (r *callerResolver) unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(context.WithValue(ctx, resolvedKey{}, r.resolve(ctx)), req)
}

// stream attaches the resolved caller to each stream's context.
func (r *callerResolver) stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	return handler(srv, resolvedStream{ServerStream: ss, ctx: context.WithValue(ctx, resolvedKey{}, r.resolve(ctx))})
}

// resolvedStream is a server stream carrying the resolved caller.
type resolvedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s resolvedStream) Context() context.Context { return s.ctx }
