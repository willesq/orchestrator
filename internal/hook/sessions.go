package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/memctx"
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

// sessionEnd writes the authoritative record and leaves the transcript for orchd
// to store. Embedding a long transcript takes seconds, longer than harnesses let
// an exit hook run, so the hook only records what is pending and returns.
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

	// Tagged with the checkout so a later prompt in the same project ranks this
	// session's work alongside the project's documentation.
	tags := detectProject(p.Cwd).sessionTags()
	if os.Getenv(agentrun.TaskIDEnv) != "" {
		tags = append(tags, memctx.QueuedTaskTag)
	}
	// Until orchd confirms them, orchd is asked to store the record and queue
	// the summary itself when it takes the transcript.
	pending := handoff.Session{ID: p.SessionID, TranscriptPath: p.TranscriptPath, Tags: tags, Record: true, Summarize: true}
	if err := handoff.PutSession(handoff.Dir(), pending); err != nil {
		return nil
	}

	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()
	// The record is small, so it is stored now when orchd answers quickly;
	// otherwise orchd stores it with the transcript.
	ctx, cancel := context.WithTimeout(context.Background(), hookRPCTimeout)
	defer cancel()
	start, end := rec.Span()
	if _, err := client.Ingest(ctx, &orchdv1.IngestRequest{
		Text: rec.Text(), SourceUri: "session://" + p.SessionID + "/record", SourceType: "session_record", Tags: tags,
		StartedAt: unixOrZero(start), EndedAt: unixOrZero(end),
	}); err == nil {
		pending.Record = false
	}
	// The excerpt is replaced by a model-written summary once orchd gets to it;
	// the request only queues it.
	sctx, scancel := context.WithTimeout(context.Background(), hookRPCTimeout)
	defer scancel()
	if resp, err := client.SummarizeSession(sctx, &orchdv1.SummarizeSessionRequest{
		SessionId: p.SessionID, TranscriptPath: p.TranscriptPath, Tags: tags,
	}); err == nil && resp.GetAccepted() {
		pending.Summarize = false
	}
	if !pending.Record || !pending.Summarize {
		_ = handoff.PutSession(handoff.Dir(), pending)
	}
	return nil
}

// hookRPCTimeout bounds each call the session-end hook makes, well inside the
// time harnesses give exit hooks, so the hook finishes on its own.
const hookRPCTimeout = 500 * time.Millisecond

// subagentStop appends a completion event to the parent session's event log.
func subagentStop(p Payload) error {
	// A parent named like a shared stream would put made-up events in the
	// digest every session reads.
	if p.ParentSessionID == "" || sharedStreams[p.ParentSessionID] {
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

// sharedStreams are event streams every session reads, never a session's own.
var sharedStreams = map[string]bool{"queue": true, "routing": true, "pr-babysitter": true}

// sessionStart prints a compact digest of recent events + pending ingests.
func sessionStart(p Payload, out io.Writer) error {
	var lines []string
	// "queue" carries finished background tasks, so their results reach the next
	// session without anyone polling the ledger.
	for _, stream := range []string{p.SessionID, "pr-babysitter", "routing", taskEventStream} {
		if stream == "" {
			continue
		}
		evs, _ := events.Read(paths.EventsDir(), stream)
		for i := max(0, len(evs)-5); i < len(evs); i++ {
			lines = append(lines, eventLine(evs[i]))
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
func parseRedacted(path string) ([]session.Message, error) { return session.ParseRedacted(path) }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// unixOrZero is t in Unix seconds, or 0 when t is unknown.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
