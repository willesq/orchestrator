package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/redact"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// The command log (spaces SP-014) stores the events toolEventIntake takes.
// See docs/internals/daemon.md "Command log".

// decisionLogged is the decision of an event no guard decided: the guard
// (SP-041) is not enforcing, so the call went ahead.
const decisionLogged = "logged"

// maxTargetsJSON caps the stored targets. Larger targets are stored as
// opaque, which a reader must treat as "may reach anything".
const maxTargetsJSON = 16 << 10

// endAnchorTimeout bounds writing a session's end record after SessionEnd.
const endAnchorTimeout = 30 * time.Second

// verifyFlushTimeout bounds how long VerifyToolLog waits for queued events
// to be stored before it checks the chain.
const verifyFlushTimeout = 10 * time.Second

// The range sessions.command_log.max_session_mb may take (SP-073), as the
// settings schema offers it.
const (
	minSessionMB = 10
	maxSessionMB = 1000
)

// logSweepEvery is how often retention runs over the command log (SP-015).
const logSweepEvery = 10 * time.Minute

// toolLog is the command log and its writer; nil when it could not be
// opened, and then events are counted and dropped as before SP-014.
type toolLog struct {
	store  *toollog.Store
	writer *toollog.Writer
	// ending holds the chains whose end record is being written, so a
	// repeated end starts no second writer wait; true means another end
	// came meanwhile and one more End runs after this one.
	endMu  sync.Mutex
	ending map[toollog.Owner]bool
	// policy is what retention keeps (SP-015).
	policy toollog.Policy
	// copies keeps a copy of each anchor and pruned head in orch.db
	// (D-35); nil without orch.db, and then verify checks the file alone.
	copies *logCopies
	// procs holds the unpinned harness processes that have a chain open
	// (D-36), at most maxProcessChains; procDropped counts the events of
	// processes refused one.
	procMu      sync.Mutex
	procs       map[toollog.Owner]bool
	procDropped int64
	// running reports whether a task runs; nil asks the current runner.
	running func(taskID string) bool
	// taskStatus reads a task's ledger status, for anchoring its chain once
	// it is terminal (D-35); nil anchors no task chain. Reason: a function
	// value, so the command log's call-graph check (toollog
	// TestCommandLogNeverReachesRAG) does not follow every Get method.
	taskStatus func(ctx context.Context, id string) (string, bool, error)
	// flushes shares one writer flush among concurrent readers.
	flushes singleflight.Group
	// reading holds the chains an agent is reading now (ListToolEvents).
	readMu  sync.Mutex
	reading map[toollog.Owner]bool
}

// openToolLog opens the command log at path and starts its writer; resolve
// picks the chain of an event whose hook named another session.
func openToolLog(path string, resolve toollog.Resolver) (*toolLog, error) {
	return openToolLogCopying(path, resolve, nil)
}

// openToolLogCopying is openToolLog that also copies every anchor and
// every retention cut into orch (orch.db) when it is not nil (D-35); see
// logCopies.copyAtOpen for what is copied at open.
func openToolLogCopying(path string, resolve toollog.Resolver, orch *sql.DB) (*toolLog, error) {
	st, err := toollog.Open(path)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	l := &toolLog{store: st, ending: map[toollog.Owner]bool{}, policy: toollog.DefaultPolicy(), procs: map[toollog.Owner]bool{}}
	open, err := st.OpenChains(ctx, toollog.OwnerProcess)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	for _, o := range open {
		l.procs[o] = true
	}
	var onAnchor func([]toollog.Anchor)
	if orch != nil {
		l.copies = newLogCopies(orch)
		if err := l.copies.copyAtOpen(ctx, st); err != nil {
			// Reason: the log still works; verify then reports the
			// anchors orch.db lacks, which is what an operator should see.
			log.Printf("orchd: command log: copying existing anchors into orch.db: %v", err)
		}
		l.copies.start()
		st.SetCutHook(l.copies.addCut)
		onAnchor = l.copies.addAnchors
	}
	l.writer = toollog.NewWriterWithAnchors(st, toollog.DefaultQueue, resolve, onAnchor)
	return l, nil
}

// logPolicy is the command log's retention from the settings. A value
// outside what the settings offer keeps its default, so a hand-edited
// setting cannot make retention delete sooner than 3 days.
func logPolicy(c config.CommandLogSettings) toollog.Policy {
	p := toollog.DefaultPolicy()
	known := false
	for _, d := range toollog.RetentionDays {
		if c.Retention == fmt.Sprintf("%dd", d) {
			p.Retention, known = time.Duration(d)*24*time.Hour, true
		}
	}
	if !known && c.Retention != "" {
		log.Printf("orchd: command log: retention %q is not 3d, 7d or 30d; keeping 7d", c.Retention)
	}
	switch {
	case c.MaxTotalMB >= 100 && c.MaxTotalMB <= 100000:
		p.TotalCap = int64(c.MaxTotalMB) << 20
	case c.MaxTotalMB != 0:
		log.Printf("orchd: command log: max_total_mb %d is outside 100-100000; keeping %d", c.MaxTotalMB, p.TotalCap>>20)
	}
	switch {
	case c.MaxSessionMB >= minSessionMB && c.MaxSessionMB <= maxSessionMB:
		p.SessionCap = int64(c.MaxSessionMB) << 20
	case c.MaxSessionMB != 0:
		log.Printf("orchd: command log: max_session_mb %d is outside %d-%d; keeping %d",
			c.MaxSessionMB, minSessionMB, maxSessionMB, p.SessionCap>>20)
	}
	return p
}

// chainState is whether a chain's owner still runs (SP-015). A session runs
// until its pin has an ended_at (SessionEnd, or the liveness sweep of
// SP-017) and its harness process is gone: any process in a session can end
// its pin early, and that must not start the clock on a log the process
// keeps writing. So an idle session left open is never expired, and a
// session ended by /clear keeps its log until its process exits. It ended
// at the later of ended_at and exited_at (when the liveness sweep saw the
// process gone); until the sweep has seen that, it still runs here. A task
// runs while the current runner runs it, and ended at its last record (its
// end record once it reached a terminal state). An unpinned harness process
// runs until it exits, and ended at its last record (D-36). A session with
// no pin ended at its last record too.
func (s *server) chainState(ctx context.Context, o toollog.Owner) (toollog.ChainState, error) {
	switch o.Kind {
	case toollog.OwnerSession:
		if s.pins == nil {
			return toollog.ChainState{}, nil
		}
		pin, ok, err := s.pins.get(ctx, o.ID)
		if err != nil || !ok {
			return toollog.ChainState{}, err
		}
		if pin.EndedAt.IsZero() || pin.ExitedAt.IsZero() || s.pins.alive(pin.Process) {
			return toollog.ChainState{Live: true}, nil
		}
		end := pin.EndedAt
		if pin.ExitedAt.After(end) {
			end = pin.ExitedAt
		}
		return toollog.ChainState{Ended: end}, nil
	case toollog.OwnerTask:
		return toollog.ChainState{Live: s.taskRunning(o.ID)}, nil
	case toollog.OwnerProcess:
		return s.processState(o), nil
	}
	return toollog.ChainState{}, fmt.Errorf("unknown owner kind %q", o.Kind)
}

func (s *server) taskRunning(id string) bool {
	if s.toolLog != nil && s.toolLog.running != nil {
		return s.toolLog.running(id)
	}
	r := s.live.get()
	if r == nil {
		return false
	}
	_, ok := r.Live(id)
	return ok
}

// sweepToolLog applies retention to the command log at time now.
func (s *server) sweepToolLog(ctx context.Context, now time.Time) {
	if s.toolLog == nil {
		return
	}
	if s.toolLog.copies != nil {
		s.toolLog.copies.flush()
	}
	// Task chains whose end no change notice anchored (D-35).
	s.endSettledTasks(ctx)
	res, err := s.toolLog.store.Sweep(ctx, now, s.toolLog.policy, s.chainState)
	if err != nil {
		// Reason: chains that failed are retried next sweep; the rest
		// were swept.
		log.Printf("orchd: command log retention: %v", err)
	}
	// Each cut is logged here, outside the file, so a pruned head written
	// into toollog.db by hand has no matching line (RC: SP-015 audit).
	for _, c := range res.Cuts {
		log.Printf("orchd: command log retention: %s %s: %d record(s) up to %d deleted, pruned head %s",
			c.Reason, c.Owner, c.Deleted, c.Seq, c.Hash)
	}
	if res.OverCap {
		log.Printf("orchd: command log retention: %d MB in use is over the %d MB cap; only running sessions and refusals under 90 days are left",
			res.Used>>20, s.toolLog.policy.TotalCap>>20)
	}
	if res.Records > 0 {
		log.Printf("orchd: command log retention: deleted %d record(s): %d ended chain(s) expired, %d running cut to size, %d deleted for the total cap; %d MB in use",
			res.Records, res.Expired, res.Trimmed, res.Capped, res.Used>>20)
	}
}

// runToolLogSweeps applies retention at start and then every interval
// until ctx ends.
func (s *server) runToolLogSweeps(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.sweepToolLog(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Close stores what is queued and closes the file.
func (l *toolLog) Close() error {
	l.writer.Close()
	if l.copies != nil {
		l.copies.close()
	}
	return l.store.Close()
}

// logToolEvent is the intake's sink. Events of callers orchd cannot tie to
// a pinned session, a running task or a harness process are not stored
// (RC-29): their owner is unknown, and storing them would let any local
// process grow the log. The intake has already counted them.
func (s *server) logToolEvent(ev toolEvent) {
	key, harness, ok := s.toolLogKey(ev)
	if !ok || !s.toolLog.admitProcess(key.Owner) {
		return
	}
	build := func() toollog.Record { return s.toolRecord(ev, harness) }
	if guardRefused(ev.req, key.Owner) {
		s.toolLog.writer.EnqueueDecision(key, eventSize(ev.req), build)
		return
	}
	s.toolLog.writer.Enqueue(key, eventSize(ev.req), build)
}

// eventSize is what an event holds in memory while it waits, for the log's
// byte budget: the whole request as received, plus room for its record.
func eventSize(req *orchdv1.RecordToolEventRequest) int64 {
	return int64(proto.Size(req)) + 512
}

// toolLogKey is whom ev is charged to and whose chain it goes to. A
// session's share of the queue belongs to its harness process, so pinning
// more sessions gains no room; its chain is the caller's pin, or the
// session the hook named when resolveClaim allows it.
func (s *server) toolLogKey(ev toolEvent) (toollog.Key, string, bool) {
	switch c := ev.caller; c.kind {
	case callerSession:
		return toollog.Key{
			Quota: fmt.Sprintf("process:%d:%d", c.pin.Process.PID, c.pin.Process.Started.UnixNano()),
			Owner: toollog.Owner{Kind: toollog.OwnerSession, ID: c.pin.SessionID},
			Claim: ev.req.GetSessionId(),
		}, c.pin.Harness, true
	case callerTask:
		if c.task.TaskID != "" {
			o := toollog.Owner{Kind: toollog.OwnerTask, ID: c.task.TaskID}
			return toollog.Key{Quota: "task:" + c.task.TaskID, Owner: o}, c.task.Provider, true
		}
	case callerNone:
		// A harness process with no pin (D-36): its events go to a chain
		// of that process, charged to it like its sessions' would be.
		if h := c.harnessProc; c.harness != "" && h.PID > 0 && !h.Started.IsZero() {
			return toollog.Key{
				Quota: fmt.Sprintf("process:%d:%d", h.PID, h.Started.UnixNano()),
				Owner: toollog.Owner{Kind: toollog.OwnerProcess, ID: toollog.ProcessID(h.PID, h.Started.UnixNano())},
			}, c.harness, true
		}
	}
	return toollog.Key{}, "", false
}

// resolveClaim is the chain of a session caller's event whose hook named
// another session: that session when it is pinned to the same harness
// process as the caller's pin (a session before /clear shares the process),
// else the caller's pin, with the claim kept as evidence (RC-27). Dropped
// events are resolved the same way, so their gap lands in the chain that
// lost them. It reads orch.db, so the log's writer calls it, never the
// hook's RPC.
func (s *server) resolveClaim(k toollog.Key) toollog.Owner {
	if s.pins == nil || k.Owner.Kind != toollog.OwnerSession {
		return k.Owner
	}
	ctx := context.Background()
	own, ok, err := s.pins.get(ctx, k.Owner.ID)
	if err != nil || !ok {
		return k.Owner
	}
	if p, ok, err := s.pins.get(ctx, k.Claim); err == nil && ok && p.Process.Same(own.Process) {
		return toollog.Owner{Kind: toollog.OwnerSession, ID: k.Claim}
	}
	return k.Owner
}

// toolRecord builds ev's record. It runs on the log's writer goroutine, so
// reading the call's targets (up to spaces' 100 ms budget) never delays the
// hook.
func (s *server) toolRecord(ev toolEvent, harness string) toollog.Record {
	req := ev.req
	f := func(v string) string { return toollog.Clean(v, toollog.MaxField) }
	// Reason: terms come out before any field is cut to its cap, so a term
	// cut in half cannot stay (SP-044).
	tv := s.termsView()
	r := toollog.Record{
		TS: ev.received.UnixNano(), HookAt: req.GetAt(),
		Harness: f(harness), Event: f(req.GetEvent()), Tool: f(req.GetToolName()),
		ClaimedSession: f(req.GetSessionId()), ToolUseID: f(req.GetToolUseId()),
		AgentID: f(req.GetAgentId()), AgentType: f(req.GetAgentType()),
		Cwd: f(tv.text(req.GetCwd())), OldCwd: f(tv.text(req.GetOldCwd())), NewCwd: f(tv.text(req.GetNewCwd())),
		InputSize: req.GetToolInputSize(),
		Decision:  decisionLogged,
	}
	if req.GetEvent() == "pre-tool-use" {
		input := req.GetToolInput()
		if len(input) == 0 && req.GetToolInputSize() > 0 {
			r.Command = fmt.Sprintf("[tool input of %d bytes was not sent]", req.GetToolInputSize())
		} else {
			r.Command = toollog.Command(req.GetToolName(), tv.json(input))
		}
		r.Targets = targetsJSON(tv.targets(spaces.Targets(req.GetToolName(), input, req.GetCwd())))
	}
	if s.spaces != nil {
		if _, rev, err := s.spaces.CurrentRevision(); err == nil {
			r.PolicyRev = rev
		}
	}
	applyHookDecision(&r, req)
	s.markGuardState(&r)
	tv.withhold(&r)
	return r
}

// targetsJSON is t as stored: redacted, and replaced by an opaque result
// when too large to keep.
func targetsJSON(t spaces.ToolTargets) string {
	b, err := json.Marshal(t)
	if err == nil && len(b) <= maxTargetsJSON {
		// Uncapped: a cut JSON document would not parse.
		out, _ := redact.Redact(string(b))
		return out
	}
	b, _ = json.Marshal(spaces.ToolTargets{Opaque: true,
		OpaqueReasons: []string{fmt.Sprintf("targets of %d bytes were not stored", len(b))}})
	return string(b)
}

// endToolLog writes a session's end record and anchors its chain head
// (SessionEnd).
func (s *server) endToolLog(id string) {
	s.endChain(toollog.Owner{Kind: toollog.OwnerSession, ID: id})
}

// endChain writes o's end record and anchors its chain head: a session's
// at SessionEnd, a task's when it reaches a terminal state, an unpinned
// process's once it exited. It runs in the background: the caller does not
// wait on it. The writer adds no end record to a chain that already ends
// in one, so a repeated end adds nothing, and a resumed session's or a
// retried task's later records are anchored at its next end.
func (s *server) endChain(o toollog.Owner) {
	if s.toolLog == nil {
		return
	}
	l := s.toolLog
	l.endMu.Lock()
	if _, busy := l.ending[o]; busy {
		// Records of a resumed session may have been queued after the End
		// in flight; anchor once more when it is done.
		l.ending[o] = true
		l.endMu.Unlock()
		return
	}
	l.ending[o] = false
	l.endMu.Unlock()
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), endAnchorTimeout)
			if err := l.writer.End(ctx, o); err != nil {
				log.Printf("orchd: command log: anchoring %s: %v", printableID(o.String()), err)
			}
			cancel()
			l.endMu.Lock()
			again := l.ending[o]
			if !again {
				delete(l.ending, o)
			} else {
				l.ending[o] = false
			}
			l.endMu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// VerifyToolLog recomputes one owner's chain. Only the operator may verify
// a chain; a session reads its own events through ListToolEvents (INV-12).
func (s *server) VerifyToolLog(ctx context.Context, req *orchdv1.VerifyToolLogRequest) (*orchdv1.VerifyToolLogResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_VerifyToolLog_FullMethodName, "read the command log"); err != nil {
		return nil, err
	}
	if s.toolLog == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "the command log is unavailable; see orchd's log")
	}
	owner, err := toollog.ParseOwner(req.GetOwner())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	if owner, err = s.toolLog.processChain(ctx, owner); err != nil {
		return nil, err
	}
	// Events already taken are checked too.
	fctx, cancel := context.WithTimeout(ctx, verifyFlushTimeout)
	defer cancel()
	if err := s.toolLog.writer.Flush(fctx); err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "command log: %v", err)
	}
	var rep toollog.Report
	if c := s.toolLog.copies; c != nil {
		// of writes the copies still waiting first, so one handed over
		// just now, or a passing hiccup in orch.db, is not reported as
		// missing. Reason for a second try: a retention cut committed
		// between reading the copies and reading the file is copied a
		// moment later, and must not read as a forged pruned head.
		for try := 0; try < 2 && (try == 0 || !rep.OK()); try++ {
			copies, cerr := c.of(ctx, owner)
			if cerr != nil {
				return nil, grpcstatus.Errorf(codes.Internal, "orch.db's anchor copies: %v", cerr)
			}
			if rep, err = s.toolLog.store.VerifyCopies(ctx, owner, copies); err != nil {
				break
			}
		}
	} else {
		rep, err = s.toolLog.store.Verify(ctx, owner)
	}
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "command log: %v", err)
	}
	out := &orchdv1.VerifyToolLogResponse{
		Records: int32(rep.Records), Anchors: int32(rep.Anchors), Ends: int32(rep.Ends),
		Gaps: int32(rep.Gaps), Dropped: rep.Dropped, Pruned: rep.Pruned,
	}
	for _, p := range rep.Problems {
		out.Problems = append(out.Problems, &orchdv1.ToolLogProblem{Seq: p.Seq, Detail: p.Detail})
	}
	return out, nil
}
