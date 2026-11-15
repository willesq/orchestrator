package hook

import (
	"context"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// toolEventTimeout bounds handing a tool event to orchd. pre-tool-use runs
// before every tool call, and the harness waits for it, so the hook gives up
// long before a person would notice. orchd only queues the event before
// answering (SP-012), so a healthy orchd answers in a few milliseconds.
const toolEventTimeout = 20 * time.Millisecond

// refusalEventTimeout bounds handing a refused call to orchd. The call is
// refused whatever orchd answers and refusals are rare, so the hook waits
// longer for the refusal to reach the command log.
const refusalEventTimeout = 250 * time.Millisecond

// maxToolInput is the largest tool input sent to orchd. A larger one, such as
// a Write of a big file, is sent as its size alone.
const maxToolInput = 1 << 20

// recordToolEvent hands a pre-tool-use or cwd-changed event, with the
// guard's decision g when one was made, to orchd for the command log
// (spaces plan SP-012, D-04, SP-041). It prints nothing. A missing orchd, a
// refusal or a timeout loses the event and nothing else: the guard has
// already decided the call.
//
// Queued tasks send their events too: orchd attributes each event to the
// caller it resolves from the connection, never to the session ID sent here.
func recordToolEvent(event string, p Payload, cf clientFactory, g *spaces.GuardResult) {
	at := time.Now()
	req := &orchdv1.RecordToolEventRequest{
		Event:         event,
		SessionId:     p.SessionID,
		Cwd:           p.Cwd,
		ToolName:      p.ToolName,
		ToolInputSize: int64(len(p.ToolInput)),
		ToolUseId:     p.ToolUseID,
		AgentId:       p.AgentID,
		AgentType:     p.AgentType,
		OldCwd:        p.OldCwd,
		NewCwd:        p.NewCwd,
		At:            at.UnixNano(),
	}
	if len(p.ToolInput) <= maxToolInput {
		req.ToolInput = p.ToolInput
	}
	wait := toolEventTimeout
	if g != nil {
		req.GuardVerdict, req.GuardRule = string(g.Would), string(g.Rule)
		req.GuardEnforced, req.GuardPolicyRev = g.Enforced, g.Revision
		if g.Terms.Known {
			// The rules revision the terms check judges under (D-66).
			rev := g.Terms.RulesRev
			if g.Terms.RulesBroken {
				// Rules orchd could not read: no revision to judge with.
				rev = -1
			}
			req.GuardRulesRev = &rev
		}
		if g.Verdict == spaces.Refuse {
			wait = refusalEventTimeout
		}
	}
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, _ = client.RecordToolEvent(ctx, req)
}
