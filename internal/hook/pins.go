package hook

import (
	"context"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// pinTimeout bounds the PinSession call. Placing a new session can ask GitHub
// about its checkout's repo, so it gets longer than the other hook calls,
// still well inside the session-start hook's budget.
const pinTimeout = 2 * time.Second

// pinSession asks orchd to pin the session at SessionStart (SP-010). orchd
// reads the harness process itself and keeps an existing pin; the source
// (startup, resume, clear, compact) only lets a resumed session's pin move to
// its new harness process (SP-017). Like every hook it never blocks the
// session: an unreachable orchd or a refusal leaves the session unpinned.
// Queued tasks are identified by their task record instead, and orchd
// refuses them the call.
func pinSession(p Payload, cf clientFactory) {
	if p.SessionID == "" || os.Getenv(agentrun.TaskIDEnv) != "" {
		return
	}
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), pinTimeout)
	defer cancel()
	_, _ = client.PinSession(ctx, &orchdv1.PinSessionRequest{
		SessionId: p.SessionID, Cwd: p.Cwd, ParentSessionId: p.ParentSessionID, Source: p.Source,
	})
}

// endSession tells orchd the session ended. The pin stays for retention.
func endSession(p Payload, cf clientFactory) {
	if p.SessionID == "" || os.Getenv(agentrun.TaskIDEnv) != "" {
		return
	}
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), hookRPCTimeout)
	defer cancel()
	_, _ = client.EndSession(ctx, &orchdv1.EndSessionRequest{SessionId: p.SessionID})
}
