package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

type fakeSummarizer struct {
	summary string
	err     error
}

func (f fakeSummarizer) Summarize(context.Context, []session.Message) (string, error) {
	return f.summary, f.err
}

type recordingIngester struct {
	metas chan rag.Meta
	texts chan string
}

func newRecordingIngester() *recordingIngester {
	return &recordingIngester{metas: make(chan rag.Meta, 4), texts: make(chan string, 4)}
}

func (r *recordingIngester) IngestText(_ context.Context, text string, m rag.Meta) (int, error) {
	r.texts <- text
	r.metas <- m
	return 1, nil
}

func transcript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"add retries"}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSummarizeSessionValidates(t *testing.T) {
	good := transcript(t)
	dir := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.jsonl")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	notJSONL := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notJSONL, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		off      bool
		full     bool
		req      *orchdv1.SummarizeSessionRequest
		code     codes.Code
		accepted bool
	}{
		{name: "accepted", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: good}, accepted: true},
		{name: "summaries off", off: true, req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: good}},
		{name: "backlog full", full: true, req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: good}},
		{name: "unsafe session id", req: &orchdv1.SummarizeSessionRequest{SessionId: "../x", TranscriptPath: good}, code: codes.InvalidArgument},
		{name: "missing file", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: "/nonexistent/s.jsonl"}, code: codes.InvalidArgument},
		{name: "relative path", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: "s.jsonl"}, code: codes.InvalidArgument},
		{name: "not a transcript", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: notJSONL}, code: codes.InvalidArgument},
		{name: "a directory", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: dir}, code: codes.InvalidArgument},
		{name: "a symlink", req: &orchdv1.SummarizeSessionRequest{SessionId: "sid", TranscriptPath: link}, code: codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{}
			if !tt.off {
				s.summaries = newSummaryQueue()
				if tt.full {
					for range summaryBacklog {
						s.summaries.enqueue(summaryJob{})
					}
				}
			}
			resp, err := s.SummarizeSession(context.Background(), tt.req)
			if tt.code != codes.OK {
				if grpcstatus.Code(err) != tt.code {
					t.Fatalf("err = %v, want %v", err, tt.code)
				}
				return
			}
			if err != nil || resp.GetAccepted() != tt.accepted {
				t.Errorf("resp = %v, err = %v; want accepted=%v", resp, err, tt.accepted)
			}
		})
	}
}

func TestSummaryReplacesTheExcerpt(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "sid", Summary: "excerpt"}); err != nil {
		t.Fatal(err)
	}
	q := newSummaryQueue()
	ing := newRecordingIngester()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := summariesUnit(q, fakeSummarizer{summary: "## Outcome\nRetries added."}, ing, time.Minute)
	if err := u.Run(ctx, func() {}); err != nil {
		t.Fatal(err)
	}
	if !q.enqueue(summaryJob{SessionID: "sid", TranscriptPath: transcript(t), Tags: []string{"checkout:/src"}}) {
		t.Fatal("enqueue refused")
	}
	select {
	case m := <-ing.metas:
		if m.SourceURI != "session://sid/record" || m.SourceType != "session_record" || !slices.Equal(m.Tags, []string{"checkout:/src"}) {
			t.Errorf("ingested %+v", m)
		}
		if got := <-ing.texts; got != "## Outcome\nRetries added." {
			t.Errorf("ingested text %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("summary never ingested")
	}
	// The record is rewritten after the ingest; wait for it rather than for a fixed time.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := session.ReadRecord(paths.SessionsDir(), "sid")
		if err == nil && rec.Summary == "## Outcome\nRetries added." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record not updated: %+v, %v", rec, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedSummaryKeepsTheExcerpt(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "sid", Summary: "excerpt"}); err != nil {
		t.Fatal(err)
	}
	ing := newRecordingIngester()
	summarizeOne(context.Background(), fakeSummarizer{err: errors.New("overloaded")}, ing,
		summaryJob{SessionID: "sid", TranscriptPath: transcript(t)}, time.Minute)
	if len(ing.metas) != 0 {
		t.Errorf("a failed summary was ingested")
	}
	if rec, _ := session.ReadRecord(paths.SessionsDir(), "sid"); rec == nil || rec.Summary != "excerpt" {
		t.Errorf("excerpt lost: %+v", rec)
	}
	// A transcript that cannot be read is skipped without calling the model.
	summarizeOne(context.Background(), fakeSummarizer{summary: "x"}, ing, summaryJob{SessionID: "sid", TranscriptPath: "/nonexistent.jsonl"}, time.Minute)
	if len(ing.metas) != 0 {
		t.Errorf("an unreadable transcript produced a summary")
	}
}

func TestNewSummarizerDefaults(t *testing.T) {
	c := newSummarizer(config.SessionSummarySettings{}, []string{"QDRANT_API_KEY"})
	if c.s.Model != "haiku" || c.s.Timeout != 2*time.Minute {
		t.Errorf("defaults = %+v", c.s)
	}
	for _, kv := range c.s.Env {
		if len(kv) > 15 && kv[:15] == "QDRANT_API_KEY=" {
			t.Errorf("summarizer inherits the vector store key")
		}
	}
	if _, err := (cliSummarizer{}).Summarize(context.Background(), nil); !errors.Is(err, errNoClaude) {
		t.Errorf("missing CLI err = %v", err)
	}
}

func TestSubsystemsIncludeTheSummarizerWhenOn(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	s := config.Settings{}
	s.RAG.Qdrant.Mode = "external"
	var names []string
	for _, u := range subsystems(deps{settings: s, summaries: newSummaryQueue()}) {
		names = append(names, u.Name)
	}
	if !slices.Contains(names, UnitSummaries) {
		t.Errorf("units = %v, want %s", names, UnitSummaries)
	}
	names = nil
	for _, u := range subsystems(deps{settings: s}) {
		names = append(names, u.Name)
	}
	if slices.Contains(names, UnitSummaries) {
		t.Errorf("summarizer present while off: %v", names)
	}
}
