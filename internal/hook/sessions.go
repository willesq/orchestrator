package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/redact"
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
	if p.TranscriptPath == "" {
		return nil
	}
	msgs, err := parseRedacted(p.TranscriptPath)
	if err != nil {
		return nil
	}
	rec := session.BuildRecord(p.SessionID, p.Cwd, p.TranscriptPath, msgs)
	// Kept for the resume brief SessionStart prints once compaction is done.
	rec.LastRequest, rec.LastReply = session.LastTurn(msgs)
	_ = session.WriteCheckpoint(paths.SessionsDir(), rec)
	return nil
}

// sessionEnd writes the authoritative record and ingests the transcript into RAG.
func sessionEnd(p Payload, cf clientFactory) error {
	if p.TranscriptPath == "" {
		return nil
	}
	msgs, err := parseRedacted(p.TranscriptPath)
	if err != nil {
		return nil
	}
	rec := session.BuildRecord(p.SessionID, p.Cwd, p.TranscriptPath, msgs)
	_ = session.WriteRecord(paths.SessionsDir(), rec)
	_ = session.DeleteCheckpoint(paths.SessionsDir(), p.SessionID)
	deleteState(p.SessionID)

	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Tagged with the checkout so a later prompt in the same project ranks this
	// session's work alongside the project's documentation.
	tags := detectProject(p.Cwd).sessionTags()
	_, _ = client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: rec.Summary, SourceUri: "session://" + p.SessionID + "/record", SourceType: "session_record", Tags: tags,
	})
	_, _ = client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: session.TranscriptText(msgs), SourceUri: "session://" + p.SessionID + "/transcript", SourceType: "session_transcript", Tags: tags,
	})
	return nil
}

// subagentStop appends a completion event to the parent session's event log.
func subagentStop(p Payload) error {
	if p.ParentSessionID == "" {
		return nil
	}
	// Redact before truncating so a secret cut at the boundary cannot slip past
	// the patterns; the digest replays this text into later sessions' context.
	summary, _ := redact.Redact(p.FinalResponse)
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
	for _, stream := range []string{p.SessionID, "pr-babysitter", taskEventStream} {
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
	if p.Source == "compact" {
		writeResumeBrief(out, p)
	}
	// Whatever exists now is covered by this digest; later prompts surface only
	// what arrives after it.
	st := loadState(p.SessionID)
	markSeen(&st, p.SessionID)
	_ = saveState(p.SessionID, st)
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
	msgs, err := parseRedacted(p.TranscriptPath)
	if err != nil {
		return nil
	}
	return session.BuildRecord(p.SessionID, p.Cwd, p.TranscriptPath, msgs)
}

// parseRedacted parses a transcript with secret-shaped substrings replaced.
// Every session derivative — the on-disk record and checkpoint as well as the RAG
// ingest — is built from these messages, and user prompts are where pasted
// tokens and .env contents most often appear. Unlike file ingest, the redaction
// threshold does not apply: a transcript is never skipped as a keystore.
func parseRedacted(path string) ([]session.Message, error) {
	msgs, err := session.ParseTranscript(path)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		msgs[i].Text, _ = redact.Redact(msgs[i].Text)
	}
	return msgs, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
