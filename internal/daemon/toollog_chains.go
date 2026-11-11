package daemon

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/queue"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// The command log's task and process chains (SP-073): a queued task's chain
// is anchored when the task reaches a terminal state (D-35), and a harness
// process holding no pin logs into a chain of its own (D-36). See
// docs/internals/daemon.md "Command log".

// maxProcessChains bounds the unpinned harness processes with a chain open
// at once. A live chain is never expired nor cut for the total cap, only to
// the per-session cap, and any process named like a harness counts as one,
// so without a bound a local process could grow the log without limit
// (RC-29); with it the process chains hold at most this many per-session
// caps. Events of further processes are dropped and counted, as before
// SP-073.
const maxProcessChains = 8

// admitProcess reports whether an event of o may be stored: always for a
// session or a task, and for a process while it has a chain open or room
// for one.
func (l *toolLog) admitProcess(o toollog.Owner) bool {
	if o.Kind != toollog.OwnerProcess {
		return true
	}
	l.procMu.Lock()
	defer l.procMu.Unlock()
	if l.procs[o] {
		return true
	}
	if len(l.procs) >= maxProcessChains {
		l.procDropped++
		return false
	}
	l.procs[o] = true
	return true
}

// processIdentity is the harness process a process chain belongs to.
func processIdentity(o toollog.Owner) (procstat.Identity, bool) {
	pid, started, ok := toollog.ParseProcessID(o.ID)
	if !ok || o.Kind != toollog.OwnerProcess {
		return procstat.Identity{}, false
	}
	return procstat.Identity{PID: pid, Started: time.Unix(0, started)}, true
}

// processState is whether an unpinned harness process's chain still runs:
// while that PID runs with the same start time. Once it exited the chain
// ended at its last record, and retention counts from there.
func (s *server) processState(o toollog.Owner) toollog.ChainState {
	proc, ok := processIdentity(o)
	if !ok || s.pins == nil {
		return toollog.ChainState{}
	}
	return toollog.ChainState{Live: s.pins.alive(proc)}
}

// endExitedProcesses ends and anchors the chain of every unpinned harness
// process that has exited or has since been pinned (its sessions log in
// their own chains from then on), and frees its place among the open ones.
func (s *server) endExitedProcesses(ctx context.Context) {
	if s.toolLog == nil || s.pins == nil {
		return
	}
	l := s.toolLog
	open, err := l.store.OpenChains(ctx, toollog.OwnerProcess)
	if err != nil {
		log.Printf("orchd: command log: listing process chains: %v", err)
		return
	}
	l.procMu.Lock()
	for o := range l.procs {
		if !slices.Contains(open, o) {
			open = append(open, o)
		}
	}
	dropped := l.procDropped
	l.procDropped = 0
	l.procMu.Unlock()
	if dropped > 0 {
		log.Printf("orchd: command log: dropped %d event(s) of unpinned harness processes: %d already have a chain open",
			dropped, maxProcessChains)
	}
	for _, o := range open {
		if s.processState(o).Live && !s.processPinned(ctx, o) {
			continue
		}
		s.endChain(o)
		l.procMu.Lock()
		delete(l.procs, o)
		l.procMu.Unlock()
	}
}

// processPinned reports whether o's process now holds a pin.
func (s *server) processPinned(ctx context.Context, o toollog.Owner) bool {
	proc, ok := processIdentity(o)
	if !ok {
		return false
	}
	_, pinned, err := s.pins.currentOf(ctx, proc)
	return err == nil && pinned
}

// processChain resolves "process:<pid>", which names no start time, to the
// chain of that PID that took an event last; any other owner is returned
// as it is.
func (l *toolLog) processChain(ctx context.Context, o toollog.Owner) (toollog.Owner, error) {
	if o.Kind != toollog.OwnerProcess {
		return o, nil
	}
	if strings.Contains(o.ID, "-") {
		if _, _, ok := toollog.ParseProcessID(o.ID); !ok {
			return toollog.Owner{}, grpcstatus.Error(codes.InvalidArgument, "a process chain is process:<pid> or process:<pid>-<start>")
		}
		return o, nil
	}
	// A PID alone: read it with any valid start time.
	pid, _, ok := toollog.ParseProcessID(o.ID + "-1")
	if !ok {
		return toollog.Owner{}, grpcstatus.Error(codes.InvalidArgument, "a process chain is process:<pid> or process:<pid>-<start>")
	}
	latest, ok, err := l.store.LatestProcessChain(ctx, pid)
	switch {
	case err != nil:
		log.Printf("orchd: command log: finding process %d's chain: %v", pid, err)
		return toollog.Owner{}, grpcstatus.Error(codes.Internal, "the command log could not be read")
	case !ok:
		return toollog.Owner{}, grpcstatus.Errorf(codes.NotFound, "the command log has no chain for process %d", pid)
	}
	return latest, nil
}

// endTaskChain anchors task id's chain if the task reached a terminal state
// (complete, failed, which includes timed out, blocked, cancelled or
// interrupted) and no run of it is live. A retried task writes on in the
// same chain and is anchored again at its next end.
func (s *server) endTaskChain(ctx context.Context, id string) {
	if s.toolLog == nil || s.toolLog.taskStatus == nil {
		return
	}
	status, ok, err := s.toolLog.taskStatus(ctx, id)
	if err != nil {
		log.Printf("orchd: command log: reading task %s: %v", printableID(id), err)
		return
	}
	// Reason: a cancelled task's session may still be exiting; the end
	// of its live run publishes the task again.
	if !ok || queue.Unsettled(status) || s.taskRunning(id) {
		return
	}
	s.endChain(toollog.Owner{Kind: toollog.OwnerTask, ID: id})
}

// endSettledTasks anchors every task chain left open whose task has reached
// a terminal state: changes missed while orchd was down or the watch fell
// behind.
func (s *server) endSettledTasks(ctx context.Context) {
	if s.toolLog == nil || s.toolLog.taskStatus == nil {
		return
	}
	open, err := s.toolLog.store.OpenChains(ctx, toollog.OwnerTask)
	if err != nil {
		log.Printf("orchd: command log: listing task chains: %v", err)
		return
	}
	for _, o := range open {
		s.endTaskChain(ctx, o.ID)
	}
}

// runTaskAnchors anchors task chains as their tasks reach a terminal state,
// following the task ledger's changes until ctx ends.
func (s *server) runTaskAnchors(ctx context.Context) {
	if s.toolLog == nil || s.toolLog.taskStatus == nil || s.hub == nil {
		return
	}
	sub, unsubscribe, ok := s.hub.subscribe()
	if !ok {
		log.Printf("orchd: command log: no room to follow task changes; task chains are anchored by the retention sweep")
		return
	}
	defer unsubscribe()
	// Tasks that ended before the subscription.
	s.endSettledTasks(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-sub.ch:
			if sub.overflow.Swap(false) {
				s.endSettledTasks(ctx)
				continue
			}
			s.endTaskChain(ctx, id)
		}
	}
}

// processRows are the session list's rows for harness processes with no
// pin (operator only): each running one, with its chain's activity, and
// with all the chains of processes that have since exited or been pinned.
func (s *server) processRows(ctx context.Context, pins []sessionPin, all bool) []*orchdv1.SessionInfo {
	chains := map[procKey]toollog.ProcessChain{}
	if s.toolLog != nil {
		list, err := s.toolLog.store.ProcessChains(ctx)
		if err != nil {
			log.Printf("orchd: ListSessions: command log: %v", err)
		}
		for _, c := range list {
			if proc, ok := processIdentity(c.Owner); ok {
				chains[keyOf(proc)] = c
			}
		}
	}
	var rows []*orchdv1.SessionInfo
	row := func(harness string, proc procstat.Identity, state string, c toollog.ProcessChain) *orchdv1.SessionInfo {
		info := &orchdv1.SessionInfo{Harness: harness, HarnessPid: int64(proc.PID), State: state,
			ToolEvents: c.Activity.Events, Refusals: c.Activity.Refused}
		if c.Activity.Last > 0 {
			info.LastActivity = time.Unix(0, c.Activity.Last).Unix()
		}
		return info
	}
	for _, p := range s.pins.unpinned(pins) {
		c := chains[keyOf(p.Identity)]
		delete(chains, keyOf(p.Identity))
		rows = append(rows, row(p.Name, p.Identity, stateUnpinned, c))
	}
	if !all {
		return rows
	}
	for _, c := range chains {
		proc, _ := processIdentity(c.Owner)
		state := stateEnded
		if s.pins.alive(proc) {
			// Its process runs and has since been pinned: these are the
			// events from before the pin.
			state = stateUnpinned
		}
		rows = append(rows, row(c.Harness, proc, state, c))
	}
	return rows
}
