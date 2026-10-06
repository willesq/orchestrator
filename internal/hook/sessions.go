package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

// stop writes a per-turn session-record checkpoint (fast, no ingest).
func stop(p Payload) error {
	rec := recordFromTranscript(p)
	if rec == nil {
		return nil
	}
	_ = session.WriteRecord(paths.SessionsDir(), rec)
	return nil
}

// preCompact writes a mid-session checkpoint sidecar before context compaction.
func preCompact(p Payload) error {
	rec := recordFromTranscript(p)
	if rec == nil {
		return nil
	}
	_ = session.WriteCheckpoint(paths.SessionsDir(), rec)
	return nil
}

// sessionEnd writes the authoritative record and ingests the transcript into RAG.
func sessionEnd(p Payload, cf clientFactory) error {
	if p.TranscriptPath == "" {
		return nil
	}
	msgs, err := session.ParseTranscript(p.TranscriptPath)
	if err != nil {
		return nil
	}
	rec := session.BuildRecord(p.SessionID, p.Cwd, p.TranscriptPath, msgs)
	_ = session.WriteRecord(paths.SessionsDir(), rec)
	_ = session.DeleteCheckpoint(paths.SessionsDir(), p.SessionID)

	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _ = client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: rec.Summary, SourceUri: "session://" + p.SessionID + "/record", SourceType: "session_record",
	})
	_, _ = client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: session.TranscriptText(msgs), SourceUri: "session://" + p.SessionID + "/transcript", SourceType: "session_transcript",
	})
	return nil
}

// subagentStop appends a completion event to the parent session's event log.
func subagentStop(p Payload) error {
	if p.ParentSessionID == "" {
		return nil
	}
	summary := p.FinalResponse
	if len(summary) > 200 {
		summary = summary[:200] + "…"
	}
	return events.Append(paths.EventsDir(), p.ParentSessionID, events.Event{
		Kind: "complete", Source: p.SubagentType, Summary: summary, Timestamp: nowRFC3339(),
	})
}

// sessionStart prints a compact digest of recent events + pending ingests.
func sessionStart(p Payload, out io.Writer) error {
	var lines []string
	// "queue" carries finished background tasks, so their results reach the next
	// session without anyone polling the ledger.
	for _, stream := range []string{p.SessionID, "pr-babysitter", "queue"} {
		if stream == "" {
			continue
		}
		evs, _ := events.Read(paths.EventsDir(), stream)
		for i := max(0, len(evs)-5); i < len(evs); i++ {
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s", evs[i].Kind, evs[i].Source, evs[i].Summary))
		}
	}
	if entries, err := os.ReadDir(paths.PendingIngestDir()); err == nil && len(entries) > 0 {
		lines = append(lines, fmt.Sprintf("- %d pending-ingest source(s) awaiting a decision", len(entries)))
	}
	if len(lines) == 0 {
		return nil
	}
	fmt.Fprintln(out, "## Orchestrator digest")
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	return nil
}

func recordFromTranscript(p Payload) *session.Record {
	if p.TranscriptPath == "" {
		return nil
	}
	msgs, err := session.ParseTranscript(p.TranscriptPath)
	if err != nil {
		return nil
	}
	return session.BuildRecord(p.SessionID, p.Cwd, p.TranscriptPath, msgs)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
