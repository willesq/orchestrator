package daemon

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// maxToolEventsPage is the most records one ListToolEvents call returns.
const maxToolEventsPage = 1000

// maxToolEventsBytes bounds one page's text for the operator, well under
// gRPC's 4 MiB message limit: a record can hold about 55 KiB.
const maxToolEventsBytes = 3 << 20

// agentToolEventsBytes bounds an agent's page. Reason: gRPC holds a reply
// until the client reads it, after the handler returns, so an agent that
// sends calls and never reads the replies makes orchd hold each page.
const agentToolEventsBytes = 512 << 10

// summaryPlaceTime bounds how long one summary spends placing directories;
// moves left after it are counted as unplaced.
var summaryPlaceTime = 20 * time.Second

// summaryWindow is the summary's window when the caller names none.
const summaryWindow = 7 * 24 * time.Hour

// maxSummaryDirs bounds how many distinct directories one summary places.
// Reason: placing a checkout can ask GitHub who owns it, and a session can
// log as many directories as it likes.
var maxSummaryDirs = 500

var errToolLogRefused = grpcstatus.Error(codes.PermissionDenied,
	"a session can read only its own command log; the operator reads any: run the command in a terminal outside any agent session")

// ListToolEvents gives a task only its own task's command log.
var _ = registerRPCs(taskRPC(orchdv1.Orchd_ListToolEvents_FullMethodName,
	spaceDecision{spaceOwn, "the caller's own command log; the operator's is everyone's (INV-12)"}))

// ListToolEvents returns a page of one owner's chain, if the caller may
// read it (INV-12).
func (s *server) ListToolEvents(ctx context.Context, req *orchdv1.ListToolEventsRequest) (*orchdv1.ListToolEventsResponse, error) {
	if s.toolLog == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "the command log is unavailable; see orchd's log")
	}
	owner, isOperator, err := s.toolLogOwner(ctx, req.GetOwner())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > maxToolEventsPage {
		limit = maxToolEventsPage
	}
	budget := maxToolEventsBytes
	if !isOperator {
		// Reason: an agent's handler reads one page of its chain at a time
		// and further calls are refused, never queued, so an agent cannot
		// pile up reads or delay the operator's. Replies gRPC still holds
		// after the handler returns are bounded by maxStreamsPerConn.
		if !s.toolLog.startRead(owner) {
			return nil, grpcstatus.Error(codes.ResourceExhausted, "a read of this command log is already running; wait for it")
		}
		defer s.toolLog.endRead(owner)
		budget = agentToolEventsBytes
	}
	// Best effort: events taken before the flush began are shown too. A
	// stalled writer must not keep what is stored from being read.
	if err := s.toolLog.flushShared(ctx); err != nil {
		log.Printf("orchd: ListToolEvents: flushing the command log: %v", err)
	}
	recs, more, pruned, err := s.toolLog.store.Page(ctx, owner, req.GetAfterSeq(), limit, budget)
	if err != nil {
		log.Printf("orchd: ListToolEvents: %v", err)
		return nil, grpcstatus.Error(codes.Internal, "the command log could not be read")
	}
	out := &orchdv1.ListToolEventsResponse{Owner: owner.String(), Pruned: pruned, More: more}
	for _, r := range recs {
		out.Events = append(out.Events, toolEventProto(r))
	}
	return out, nil
}

// toolLogOwner is the chain the caller asked for, if it may read it, and
// whether the caller is the operator: the operator reads any (it must name
// one), a pinned session only its own, a queued task only its own task's.
// Anyone else, or a request for another chain, is refused alike, so the
// answer never says whether another session exists.
func (s *server) toolLogOwner(ctx context.Context, requested string) (toollog.Owner, bool, error) {
	if _, ok := operator(ctx); ok {
		if requested == "" {
			return toollog.Owner{}, true, grpcstatus.Error(codes.InvalidArgument, "name a session ID, task:<task id> or process:<pid>")
		}
		o, err := toollog.ParseOwner(requested)
		if err != nil {
			return toollog.Owner{}, true, grpcstatus.Error(codes.InvalidArgument, err.Error())
		}
		o, err = s.toolLog.processChain(ctx, o)
		return o, true, err
	}
	own, err := s.callerChain(ctx)
	if err != nil {
		return toollog.Owner{}, false, err
	}
	if requested == "" {
		return own, false, nil
	}
	o, err := toollog.ParseOwner(requested)
	if err != nil {
		return toollog.Owner{}, false, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	if o != own {
		log.Printf("orchd: refused ListToolEvents of another chain from %s", printableID(own.String()))
		return toollog.Owner{}, false, errToolLogRefused
	}
	return own, false, nil
}

// startRead claims o's one agent read; false when one is running.
func (l *toolLog) startRead(o toollog.Owner) bool {
	l.readMu.Lock()
	defer l.readMu.Unlock()
	if l.reading[o] {
		return false
	}
	if l.reading == nil {
		l.reading = map[toollog.Owner]bool{}
	}
	l.reading[o] = true
	return true
}

func (l *toolLog) endRead(o toollog.Owner) {
	l.readMu.Lock()
	defer l.readMu.Unlock()
	delete(l.reading, o)
}

// callerChain is the chain an agent caller owns. A session's is the current
// session of the harness process its resolved pin names (RC-27), and only
// while that pin is still there: a connection keeps the pin it resolved,
// so a pin a resume has since moved to another process (RC-50), or a
// session /clear has since replaced, is no longer the caller's.
func (s *server) callerChain(ctx context.Context) (toollog.Owner, error) {
	switch c := callerOf(ctx); c.kind {
	case callerSession:
		if s.pins == nil {
			break
		}
		p, ok, err := s.pins.currentOf(ctx, c.pin.Process)
		if err != nil {
			log.Printf("orchd: ListToolEvents: %v", err)
			return toollog.Owner{}, grpcstatus.Error(codes.Internal, "the session pins could not be read")
		}
		if ok && p.SessionID == c.pin.SessionID {
			return toollog.Owner{Kind: toollog.OwnerSession, ID: p.SessionID}, nil
		}
	case callerTask:
		if c.task.TaskID != "" {
			return toollog.Owner{Kind: toollog.OwnerTask, ID: c.task.TaskID}, nil
		}
	}
	return toollog.Owner{}, errToolLogRefused
}

// flushShared waits until the writer has stored the events it took before
// the flush began. Concurrent readers share one flush, so readers cannot
// pile control jobs on the writer's queue, which per-owner shares do not
// bound; a reader that joins a running flush may miss events taken just
// before it called.
func (l *toolLog) flushShared(ctx context.Context) error {
	ch := l.flushes.DoChan("flush", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.Background(), verifyFlushTimeout)
		defer cancel()
		return nil, l.writer.Flush(fctx)
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func toolEventProto(r toollog.Record) *orchdv1.ToolEvent {
	return &orchdv1.ToolEvent{
		Seq: r.Seq, At: r.TS, Kind: r.Kind, Harness: r.Harness, Event: r.Event, Tool: r.Tool,
		AgentId: r.AgentID, AgentType: r.AgentType, Cwd: r.Cwd, OldCwd: r.OldCwd, NewCwd: r.NewCwd,
		Command: r.Command, Targets: r.Targets, Decision: r.Decision, Rule: r.Rule, Dropped: r.Dropped,
		ClaimedSession: r.ClaimedSession,
	}
}

// Reason: the summary names every space and its activity.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SummarizeToolLog_FullMethodName, "summarize the command log",
	spaceDecision{spaceOperator, "every space's activity"}))

// SummarizeToolLog sums the command log per space over a window. It names
// every space and so is the operator's alone.
func (s *server) SummarizeToolLog(ctx context.Context, req *orchdv1.SummarizeToolLogRequest) (*orchdv1.ToolLogSummary, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SummarizeToolLog_FullMethodName, "summarize the command log"); err != nil {
		return nil, err
	}
	if s.toolLog == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "the command log is unavailable; see orchd's log")
	}
	now := time.Now()
	since := now.Add(-summaryWindow)
	switch v := req.GetSince(); {
	case v < 0 || v > now.Unix():
		return nil, grpcstatus.Error(codes.InvalidArgument, "since must be a time in the past")
	case v > 0:
		since = time.Unix(v, 0)
	}
	acts, err := s.toolLog.store.ActivitySince(ctx, since.UnixNano())
	if err != nil {
		log.Printf("orchd: SummarizeToolLog: %v", err)
		return nil, grpcstatus.Error(codes.Internal, "the command log could not be read")
	}
	// Reason: the directories are whatever sessions reported, so placing
	// them must not spend the GitHub lookups pinning sessions relies on,
	// nor take long.
	pctx, cancel := context.WithTimeout(withCachedRepoLookups(ctx), summaryPlaceTime)
	defer cancel()
	sum := &toolSummary{s: s, placeCtx: pctx, rows: map[string]*orchdv1.SpaceToolActivity{}, placed: map[string]placedDir{}}
	for _, a := range acts {
		if err := sum.add(ctx, a); err != nil {
			return nil, err
		}
	}
	out := &orchdv1.ToolLogSummary{Since: since.Unix(), Until: now.Unix(), UnplacedMoves: sum.unplaced}
	for _, r := range sum.rows {
		out.Spaces = append(out.Spaces, r)
	}
	sort.Slice(out.Spaces, func(i, j int) bool { return out.Spaces[i].Space < out.Spaces[j].Space })
	return out, nil
}

// toolSummary builds one SummarizeToolLog answer.
type toolSummary struct {
	s *server
	// placeCtx bounds placing directories, apart from reading pins.
	placeCtx context.Context
	rows     map[string]*orchdv1.SpaceToolActivity
	// placed is the space a session would be pinned to in each directory
	// tried so far; ok is false for one that could not be placed.
	placed   map[string]placedDir
	unplaced int64
}

type placedDir struct {
	space string
	ok    bool
}

// add counts one owner's activity in the space it is pinned to.
func (t *toolSummary) add(ctx context.Context, a toollog.OwnerActivity) error {
	space, err := t.spaceOf(ctx, a.Owner)
	if err != nil {
		return err
	}
	r := t.rows[space]
	if r == nil {
		r = &orchdv1.SpaceToolActivity{Space: space}
		t.rows[space] = r
	}
	r.Sessions++
	r.ToolCalls += a.Calls
	r.Refusals += a.Refused
	for dir, n := range a.Moves {
		r.DirectoryMoves += n
		if space == "" {
			continue
		}
		to, ok := t.place(t.placeCtx, dir)
		switch {
		case !ok:
			t.unplaced += n
		case to != "" && to != space:
			r.CrossSpaceMoves += n
		}
	}
	return nil
}

// spaceOf is the space o's events belong to: a session's pinned space; ""
// for a task, which has no space yet, or a session with no pin.
func (t *toolSummary) spaceOf(ctx context.Context, o toollog.Owner) (string, error) {
	if o.Kind != toollog.OwnerSession || t.s.pins == nil {
		return "", nil
	}
	p, ok, err := t.s.pins.get(ctx, o.ID)
	if err != nil {
		log.Printf("orchd: SummarizeToolLog: %v", err)
		return "", grpcstatus.Error(codes.Internal, "the session pins could not be read")
	}
	if !ok {
		return "", nil
	}
	return p.Space, nil
}

// place is the space a session started in dir would be pinned to now;
// false when dir is past the summary's budget or could not be placed.
func (t *toolSummary) place(ctx context.Context, dir string) (string, bool) {
	if p, ok := t.placed[dir]; ok {
		return p.space, p.ok
	}
	if len(t.placed) >= maxSummaryDirs || !strings.HasPrefix(dir, "/") || ctx.Err() != nil {
		return "", false
	}
	sp, _, err := t.s.placeSession(ctx, dir)
	if err != nil {
		log.Printf("orchd: SummarizeToolLog: placing a directory: %v", err)
	}
	t.placed[dir] = placedDir{sp, err == nil}
	return sp, err == nil
}
