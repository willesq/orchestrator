package hook

import (
	"context"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// toolEventTimeout bounds handing a tool event to orchd. pre-tool-use runs
// before every tool call, and the harness waits for it, so the hook gives up
// long before a person would notice. orchd only queues the event before
// answering (SP-012), so a healthy orchd answers in a few milliseconds.
const toolEventTimeout = 20 * time.Millisecond

// maxToolInput is the largest tool input sent to orchd. A larger one, such as
// a Write of a big file, is sent as its size alone.
const maxToolInput = 1 << 20

// recordToolEvent hands a pre-tool-use or cwd-changed event to orchd for the
// command log (spaces plan SP-012, D-04). It prints nothing, which every
// harness reads as "no decision", so the call goes ahead as it would without
// the hook. A missing orchd, a refusal or a timeout loses the event and
// nothing else. Enforcement (SP-041) is added on top of this.
//
// Queued tasks send their events too: orchd attributes each event to the
// caller it resolves from the connection, never to the session ID sent here.
func recordToolEvent(event string, p Payload, cf clientFactory) {
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
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), toolEventTimeout)
	defer cancel()
	_, _ = client.RecordToolEvent(ctx, req)
}
