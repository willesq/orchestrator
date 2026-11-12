package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/summarize"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// UnitSummaries is the supervised unit that writes session summaries.
const UnitSummaries = "session-summarizer"

// summaryBacklog bounds sessions waiting for a summary. Summaries are a
// best-effort upgrade over the excerpt already recorded, so a burst beyond it
// is dropped rather than queued without limit.
const summaryBacklog = 32

// summaryJob is one finished session waiting for its summary.
type summaryJob struct {
	SessionID      string
	TranscriptPath string
	Tags           []string
}

// sessionSummarizer turns a session's messages into its summary.
type sessionSummarizer interface {
	Summarize(ctx context.Context, msgs []session.Message) (string, error)
}

// summaryIngester stores a summary in the vector store.
type summaryIngester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// summaryQueue holds sessions waiting for a summary. The handler only enqueues,
// so a session's end never waits on a model call; the unit drains it.
type summaryQueue struct {
	jobs chan summaryJob
}

func newSummaryQueue() *summaryQueue {
	return &summaryQueue{jobs: make(chan summaryJob, summaryBacklog)}
}

// enqueue reports whether the job was queued; false means the backlog is full.
func (q *summaryQueue) enqueue(j summaryJob) bool {
	select {
	case q.jobs <- j:
		return true
	default:
		return false
	}
}

// summariesUnit drains the queue for as long as it runs. A job taken when the
// unit stops is abandoned; the session keeps its excerpt.
func summariesUnit(q *summaryQueue, s sessionSummarizer, ing summaryIngester, timeout time.Duration) supervisor.Unit {
	return supervisor.Unit{
		Name:   UnitSummaries,
		Detail: "model-written summaries of finished sessions",
		Run: func(ctx context.Context, beat func()) error {
			beat()
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case j := <-q.jobs:
						summarizeOne(ctx, s, ing, j, timeout)
						beat()
					}
				}
			}()
			return nil
		},
	}
}

// summarizeOne replaces a session's excerpt with its summary, in the vector
// store and in the session record on disk. Any failure leaves the excerpt.
func summarizeOne(ctx context.Context, s sessionSummarizer, ing summaryIngester, j summaryJob, timeout time.Duration) {
	// Redacted like everything else that reaches the store: the summary is
	// stored, and the model writing it sees only what it needs.
	msgs, err := session.ParseRedacted(j.TranscriptPath)
	if err != nil || len(msgs) == 0 {
		return
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	summary, err := s.Summarize(jobCtx, msgs)
	if err != nil {
		log.Printf("session-summarizer: session %s: summary skipped: %v", j.SessionID, err)
		return
	}
	// On disk first, under the session's lock: a hand-off storing the record
	// reads it from there, so it can never store the excerpt over this.
	defer lockSession(j.SessionID)()
	rec, err := session.ReadRecord(paths.SessionsDir(), j.SessionID)
	if err != nil {
		rec = &session.Record{SessionID: j.SessionID}
	}
	rec.Summary = summary
	if rec.StartedAt == "" && rec.EndedAt == "" {
		rec.SetSpan(session.Span(msgs))
	}
	if err == nil {
		_ = session.WriteRecord(paths.SessionsDir(), rec)
	}
	start, end := rec.Span()
	if _, err := ing.IngestText(jobCtx, rec.Text(), rag.Meta{
		SourceURI: "session://" + j.SessionID + "/record", SourceType: "session_record", Tags: j.Tags,
		StartedAt: start, EndedAt: end, Writer: rag.WriterSession, Origin: sessionOrigin(j.SessionID, rec.Cwd, j.TranscriptPath),
	}); err != nil {
		log.Printf("session-summarizer: session %s: store summary: %v", j.SessionID, err)
	}
}

// newSummarizer builds the CLI-backed summarizer from settings.
func newSummarizer(s config.SessionSummarySettings, dropEnv []string) cliSummarizer {
	home, _ := os.UserHomeDir()
	model := s.Model
	if model == "" {
		model = "haiku"
	}
	return cliSummarizer{s: summarize.Summarizer{
		Bin:     agentrun.FindClaude("", home),
		Model:   model,
		Timeout: interval(s.Timeout, 2*time.Minute),
		Env:     summarize.Env(os.Environ(), dropEnv...),
	}}
}

// cliSummarizer runs each summary in a fresh empty directory, so the call has
// no project to discover.
type cliSummarizer struct{ s summarize.Summarizer }

func (c cliSummarizer) Summarize(ctx context.Context, msgs []session.Message) (string, error) {
	if c.s.Bin == "" {
		return "", errNoClaude
	}
	dir, cleanup, err := summarize.TempDir()
	if err != nil {
		return "", err
	}
	defer cleanup()
	s := c.s
	s.Dir = dir
	return s.Summarize(ctx, msgs)
}

var errNoClaude = grpcstatus.Error(codes.FailedPrecondition, "no Claude Code CLI found")

// sessionIDShape is what a harness session ID looks like; it names files.
var sessionIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SummarizeSession queues a finished session for its summary and returns at
// once. The transcript path must name an existing .jsonl file, so the RPC cannot
// be used to feed arbitrary files to the model and into the store.
func (s *server) SummarizeSession(_ context.Context, req *orchdv1.SummarizeSessionRequest) (*orchdv1.SummarizeSessionResponse, error) {
	if s.summaries == nil {
		return &orchdv1.SummarizeSessionResponse{Accepted: false}, nil
	}
	if !sessionIDShape.MatchString(req.GetSessionId()) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "invalid session id")
	}
	p := req.GetTranscriptPath()
	if !filepath.IsAbs(p) || !strings.HasSuffix(p, ".jsonl") {
		return nil, grpcstatus.Error(codes.InvalidArgument, "transcript_path must be an absolute .jsonl path")
	}
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return nil, grpcstatus.Error(codes.InvalidArgument, "transcript_path is not a regular file")
	}
	ok := s.summaries.enqueue(summaryJob{SessionID: req.GetSessionId(), TranscriptPath: p, Tags: req.GetTags()})
	return &orchdv1.SummarizeSessionResponse{Accepted: ok}, nil
}
