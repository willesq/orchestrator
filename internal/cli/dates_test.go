package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

func TestSearchAndSourcesShowDates(t *testing.T) {
	at := time.Date(2026, 10, 21, 14, 2, 0, 0, time.UTC).Unix()
	fc := &fakeClient{
		search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
			{SourceUri: "session://a/transcript", SourceType: "session_transcript", StartedAt: at, EndedAt: at, Text: "t"},
		}},
		listSources: &orchdv1.ListSourcesResponse{TotalSources: 1, TotalChunks: 1, Sources: []*orchdv1.SourceSummary{
			{SourceUri: "session://b/record", SourceType: "session_record", Chunks: 1},
		}},
	}
	cf := factory(fc, nil)
	out, err := run(cf, "", "search", "--since", "yesterday", "--until", "today", "what", "happened")
	if err != nil || !strings.Contains(out, "(session_transcript) · 2026-10-21 14:02 UTC") || fc.searchReq.GetSince() == 0 || fc.searchReq.GetUntil() <= fc.searchReq.GetSince() {
		t.Fatalf("search: %q, %v, req %+v", out, err, fc.searchReq)
	}
	if _, err := run(cf, "", "search", "--since", "someday", "q"); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("bad --since err = %v", err)
	}
	out, err = run(cf, "", "rag", "sources", "--until", "2026-10-21")
	if err != nil || !strings.Contains(out, "when=date unknown") || fc.listReq.GetUntil() == 0 {
		t.Fatalf("sources: %q, %v", out, err)
	}
	if _, err := run(cf, "", "rag", "sources", "--until", "x"); err == nil || !strings.Contains(err.Error(), "--until") {
		t.Fatalf("bad --until err = %v", err)
	}
}

func TestBackfillSessions(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	t.Setenv(agentrun.TaskIDEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*session.Record{
		{SessionID: "todo", TranscriptPath: tr},
		{SessionID: "gone", TranscriptPath: "/nonexistent/t.jsonl"},
		{SessionID: "dated", TranscriptPath: tr},
	} {
		if err := session.WriteRecord(paths.SessionsDir(), r); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeClient{listSources: &orchdv1.ListSourcesResponse{Sources: []*orchdv1.SourceSummary{
		{SourceUri: "session://todo/transcript", Tags: []string{"other"}},
		{SourceUri: "session://todo/record", Tags: []string{"checkout:/x"}},
		{SourceUri: "session://gone/record"},
		{SourceUri: "session://norecord/record"},
		{SourceUri: "session://dated/transcript", StartedAt: 100},
		{SourceUri: "session://../evil/record"},
		{SourceUri: "manual://x"},
	}}}
	cf := factory(fc, nil)
	out, err := run(cf, "", "rag", "backfill-sessions")
	if err != nil || !strings.Contains(out, "4 session(s) in memory: 1 to store again with dates, 1 already dated, 2 without a transcript on disk") ||
		!strings.Contains(out, "plan only") || fc.listReq.GetUriPrefix() != "session://" {
		t.Fatalf("plan: %q, %v", out, err)
	}
	if left, _ := handoff.Sessions(handoff.Dir(), time.Now().Add(time.Hour)); len(left) != 0 {
		t.Fatal("a plan handed sessions off")
	}
	out, err = run(cf, "", "rag", "backfill-sessions", "--apply")
	if err != nil || !strings.Contains(out, "handed 1 session(s) to orchd") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	left, _ := handoff.Sessions(handoff.Dir(), time.Now().Add(time.Hour))
	if len(left) != 1 || left[0].ID != "todo" || left[0].TranscriptPath != tr || !left[0].Record || left[0].Summarize ||
		len(left[0].Tags) != 1 || left[0].Tags[0] != "checkout:/x" {
		t.Fatalf("handed off %+v", left)
	}
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	if _, err := run(cf, "", "rag", "backfill-sessions", "--apply"); err == nil {
		t.Fatal("a queue task backfilled")
	}
}
