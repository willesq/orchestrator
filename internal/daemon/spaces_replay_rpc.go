package daemon

import (
	"context"
	"errors"
	"log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// Bounds on one replay. Reason: a chain can hold days of events, and the
// reply must stay well under gRPC's 4 MiB message limit.
var (
	maxReplayEvents = 50000
	maxReplayRows   = 5000
)

// replayModes are the guard modes a replay may judge in.
var replayModes = map[string]bool{
	string(spaces.GuardOff): true, string(spaces.GuardLog): true,
	string(spaces.GuardBlockOtherJobs): true, string(spaces.GuardStrict): true,
}

// Reason (SP-047): a replay reads any session's command log and every
// policy revision; it is also refused to interactive harness sessions
// (refuseNonOperator).
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ReplaySpaces_FullMethodName, "replay the command log",
	spaceDecision{spaceOperator, "any session's command log"}))

// ReplaySpaces decides one chain's recorded events again (SP-047). It reads
// the command log, the owner's pin and stored revisions, and writes nothing.
func (s *server) ReplaySpaces(ctx context.Context, req *orchdv1.ReplaySpacesRequest) (*orchdv1.ReplaySpacesResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ReplaySpaces_FullMethodName, "replay the command log"); err != nil {
		return nil, err
	}
	if s.toolLog == nil || s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "the command log or the spaces policy is unavailable; see orchd's log")
	}
	if req.GetPolicyRevision() < 0 || req.GetPolicyRevision() > 0 && req.GetPolicyJson() != "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name at most one policy: a revision number or a policy document")
	}
	mode := req.GetMode()
	if mode != "" && !replayModes[mode] {
		return nil, grpcstatus.Error(codes.InvalidArgument, "mode must be off, log, block-other-jobs or strict")
	}
	if mode == "" {
		mode = s.enforcement.state(spaces.LayerGuard)
	}
	policies, err := s.replayPolicies(ctx, req)
	if err != nil {
		return nil, err
	}
	owner, err := toollog.ParseOwner(req.GetOwner())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	if owner, err = s.toolLog.processChain(ctx, owner); err != nil {
		return nil, err
	}
	pin, err := s.replayPin(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ReplaySpacesResponse{Owner: owner.String(), Mode: mode, PinSpace: pin.Space, Pinned: pin.Found}
	// Reason: a terms refusal is carried only while the terms layer is
	// on, as the hook checks terms only then (RC-116).
	enf := spaces.GuardEnforcement{Guard: spaces.GuardMode(mode), Terms: s.enforcement.state(spaces.LayerTerms)}
	// Reason: each event is judged with the outbound rules revision it
	// was recorded with (SP-075, D-66), not today's.
	rules, err := s.termsRules.replayRules(ctx)
	if err != nil {
		log.Printf("orchd: ReplaySpaces: %v", err)
		return nil, grpcstatus.Error(codes.Internal, "the outbound rules could not be read")
	}
	err = s.replayPages(ctx, owner, func(events []spaces.ReplayEvent) {
		rep := spaces.ReplayWithRules(events, pin, enf, policies, rules)
		out.Events += int64(len(rep.Rows))
		out.Compared += int64(rep.Compared)
		out.Changed += int64(rep.Changed)
		out.Unrecorded += int64(rep.Unrecorded)
		out.Problems += int64(rep.Problems)
		for _, r := range rep.Rows {
			if !req.GetAll() && !r.Changed && r.Problem == "" {
				continue
			}
			if len(out.Rows) >= maxReplayRows {
				out.Truncated = true
				break
			}
			out.Rows = append(out.Rows, replayRowProto(r))
		}
	})
	if errors.Is(err, errReplayTruncated) {
		out.Truncated, err = true, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// replayPolicies is the policy each event is decided under: the one the
// request names, or the revision recorded with the event.
func (s *server) replayPolicies(ctx context.Context, req *orchdv1.ReplaySpacesRequest) (spaces.ReplayPolicies, error) {
	guard := func(res *spaces.Resolved) (spaces.GuardPolicy, error) {
		// Reason: the protection the live guard's snapshot carries, so a
		// recorded self-protection refusal replays as one (RC-110).
		return spaces.NewGuardPolicyProtecting(res, s.guardProtection(res))
	}
	var fixed *spaces.Resolved
	switch {
	case req.GetPolicyJson() != "":
		p, err := spaces.Parse([]byte(req.GetPolicyJson()))
		if err != nil {
			return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
		}
		if fixed, err = s.spaces.Resolve(p); err != nil {
			return nil, grpcstatus.Error(codes.InvalidArgument, "the policy does not validate: "+err.Error())
		}
	case req.GetPolicyRevision() > 0:
		res, err := s.spaces.ResolvedRevision(ctx, req.GetPolicyRevision())
		if err != nil {
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "revision %d cannot be replayed: %v", req.GetPolicyRevision(), err)
		}
		fixed = res
	default:
		// Reason: events are replayed a page at a time, so each revision is
		// read and validated once for the whole replay, not once a page.
		type built struct {
			pol spaces.GuardPolicy
			err error
		}
		cache := map[int64]built{}
		return func(rev int64) (spaces.GuardPolicy, error) {
			if b, ok := cache[rev]; ok {
				return b.pol, b.err
			}
			var b built
			res, err := s.spaces.ResolvedRevision(ctx, rev)
			if b.err = err; err == nil {
				b.pol, b.err = guard(res)
			}
			cache[rev] = b
			return b.pol, b.err
		}, nil
	}
	pol, err := guard(fixed)
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	return func(int64) (spaces.GuardPolicy, error) { return pol, nil }, nil
}

// replayPin is the owner's pin as the guard reads it: a session's recorded
// pin, or the pin of a task's agent session, which orchd takes for it as
// the task runs (SP-029); harness processes hold none.
func (s *server) replayPin(ctx context.Context, owner toollog.Owner) (spaces.GuardPin, error) {
	id := owner.ID
	switch {
	case s.pins == nil:
		return spaces.GuardPin{}, nil
	case owner.Kind == toollog.OwnerTask:
		// Reason: the guard reads the pin file of the session the hook
		// names, which for a task is its agent session (a Codex task's
		// thread is known only after it ends, so it has none).
		id = agentrun.SessionID(owner.ID)
	case owner.Kind != toollog.OwnerSession:
		return spaces.GuardPin{}, nil
	}
	p, ok, err := s.pins.get(ctx, id)
	if ok && owner.Kind == toollog.OwnerTask && p.InheritedFrom != taskPinOwner(owner.ID) {
		// Reason: only the pin orchd took for this task judges its calls;
		// another under that ID (a Codex task's, which has none) does not.
		ok, p = false, sessionPin{}
	}
	if err != nil {
		if ctx.Err() != nil {
			return spaces.GuardPin{}, grpcstatus.FromContextError(ctx.Err()).Err()
		}
		log.Printf("orchd: ReplaySpaces: reading the pin: %v", err)
		return spaces.GuardPin{}, grpcstatus.Error(codes.Internal, "the session's pin could not be read")
	}
	return spaces.GuardPin{Found: ok, Space: p.Space}, nil
}

// errReplayTruncated stops a replay at maxReplayEvents.
var errReplayTruncated = errors.New("replay truncated")

// replayPages hands owner's stored tool events to decide one page at a time,
// in sequence order, at most maxReplayEvents of them (then
// errReplayTruncated). Reason: a chain can hold tens of thousands of events
// with their targets, so they are decided and dropped a page at a time
// rather than held at once.
func (s *server) replayPages(ctx context.Context, owner toollog.Owner, decide func([]spaces.ReplayEvent)) error {
	if err := s.toolLog.flushShared(ctx); err != nil {
		log.Printf("orchd: ReplaySpaces: flushing the command log: %v", err)
	}
	var after int64
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return grpcstatus.FromContextError(err).Err()
		}
		recs, more, _, err := s.toolLog.store.Page(ctx, owner, after, maxToolEventsPage, maxToolEventsBytes)
		if err != nil {
			log.Printf("orchd: ReplaySpaces: %v", err)
			return grpcstatus.Error(codes.Internal, "the command log could not be read")
		}
		var events []spaces.ReplayEvent
		full := false
		for _, r := range recs {
			after = r.Seq
			if r.Kind != toollog.KindEvent {
				continue
			}
			if seen >= maxReplayEvents {
				full = true
				break
			}
			seen++
			events = append(events, spaces.ReplayEvent{
				Seq: r.Seq, At: r.TS, PolicyRev: r.PolicyRev, RulesRev: r.RulesRev, Command: r.Command,
				Guard:    spaces.GuardEventOf(r.Event, r.Tool, r.Cwd, r.OldCwd, r.NewCwd, r.Targets),
				Recorded: spaces.RecordedVerdict(r.Decision, r.Rule),
			})
		}
		decide(events)
		switch {
		case full:
			return errReplayTruncated
		case !more || len(recs) == 0:
			return nil
		}
	}
}

func replayRowProto(r spaces.ReplayRow) *orchdv1.ReplayedEvent {
	ev := r.Event
	return &orchdv1.ReplayedEvent{
		Seq: ev.Seq, At: ev.At, Event: string(ev.Guard.Kind), Tool: ev.Guard.Tool, PolicyRevision: ev.PolicyRev,
		Recorded: ev.Recorded.OK, RecordedVerdict: string(ev.Recorded.Verdict), RecordedRule: string(ev.Recorded.Rule),
		Verdict: string(r.Verdict), Rule: string(r.Rule), Changed: r.Changed, Problem: r.Problem,
	}
}
