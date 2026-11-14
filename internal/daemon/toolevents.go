package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// maxToolEventInput matches the hook's own limit (internal/hook maxToolInput).
const maxToolEventInput = 1 << 20

// maxToolEventField caps every other string field. A path or ID is far
// shorter; without it only gRPC's 4 MiB message limit bounds them (RC-29).
const maxToolEventField = 4 << 10

// toolEvent is one pre-tool-use or cwd-changed event with the caller orchd
// resolved for it (SP-011). The request's session_id is only the harness's
// claim; caller says whose event it is.
type toolEvent struct {
	caller   resolvedCaller
	req      *orchdv1.RecordToolEventRequest
	received time.Time
	// rulesRev is the outbound rules revision orchd applied when the
	// event arrived (D-66). Reason: taken at receipt, since the record is
	// built later by the log's writer, after a change may have landed.
	rulesRev int64
}

// toolEventIntake takes the events the tool hooks hand off (spaces plan
// SP-012). It answers as soon as an event is taken, so a hook that waits on
// RecordToolEvent waits for nothing else. The command log (SP-014) is the
// sink; without it events are counted and dropped. Nothing is ever indexed.
// The zero value is ready to use.
type toolEventIntake struct {
	mu sync.RWMutex
	// sink receives each event; it must not block. nil drops events.
	sink func(toolEvent)
	// received counts events by caller kind, for tests and diagnostics.
	received sync.Map // callerKind -> *atomic.Int64
}

func (t *toolEventIntake) take(ev toolEvent) {
	n, _ := t.received.LoadOrStore(ev.caller.kind, new(atomic.Int64))
	n.(*atomic.Int64).Add(1)
	t.mu.RLock()
	sink := t.sink
	t.mu.RUnlock()
	if sink != nil {
		sink(ev)
	}
}

// count returns how many events callers of kind handed off.
func (t *toolEventIntake) count(kind callerKind) int64 {
	n, ok := t.received.Load(kind)
	if !ok {
		return 0
	}
	return n.(*atomic.Int64).Load()
}

func (t *toolEventIntake) setSink(sink func(toolEvent)) {
	t.mu.Lock()
	t.sink = sink
	t.mu.Unlock()
}

// RecordToolEvent takes one tool hook event. Any caller may send one: the
// event is attributed to the caller resolved from the connection, so a
// request cannot claim another session's events.
func (s *server) RecordToolEvent(ctx context.Context, req *orchdv1.RecordToolEventRequest) (*orchdv1.RecordToolEventResponse, error) {
	switch req.GetEvent() {
	case "pre-tool-use", "cwd-changed":
	default:
		return nil, grpcstatus.Error(codes.InvalidArgument, "event must be pre-tool-use or cwd-changed")
	}
	// An empty session ID is kept: a payload the hook could not parse still
	// shows that a tool call happened.
	if id := req.GetSessionId(); id != "" && !sessionIDPattern.MatchString(id) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session IDs are 1 to 128 letters, digits, '-' or '_'")
	}
	if len(req.GetToolInput()) > maxToolEventInput {
		return nil, grpcstatus.Error(codes.InvalidArgument, "tool_input is over 1 MiB; send its size alone")
	}
	// Reason: fields this orchd does not know would be held in memory
	// with the request without being counted or stored.
	if len(req.ProtoReflect().GetUnknown()) > 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "the request has fields this orchd does not know")
	}
	for _, f := range []string{req.GetCwd(), req.GetToolName(), req.GetToolUseId(), req.GetAgentId(),
		req.GetAgentType(), req.GetOldCwd(), req.GetNewCwd()} {
		if len(f) > maxToolEventField {
			return nil, grpcstatus.Error(codes.InvalidArgument, "a tool event field other than tool_input is over 4 KiB")
		}
	}
	s.toolEvents.take(toolEvent{caller: callerOf(ctx), req: req, received: time.Now(),
		rulesRev: s.termsRules.recordedRulesRev(req.GuardRulesRev)})
	return &orchdv1.RecordToolEventResponse{}, nil
}
