package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestSearchDatesAndRanges(t *testing.T) {
	at := time.Date(2026, 10, 21, 14, 2, 0, 0, time.UTC).Unix()
	fc := &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{SourceUri: "session://a/transcript", SourceType: "session_transcript", StartedAt: at, EndedAt: at + 3600, Text: "t1"},
		{SourceUri: "session://b/record", SourceType: "session_record", Text: "t2"},
		{SourceUri: "repo://x/README.md", SourceType: "repo_doc", Text: "t3"},
	}}}
	rt := ragTools{client: fc}
	res, err := rt.search(context.Background(), toolReq(map[string]any{"query": "q", "since": "2026-10-21", "until": "2026-10-21"}))
	if err != nil || res.IsError {
		t.Fatalf("search = %v, %v", resultText(res), err)
	}
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if fc.searchReq.GetSince() != day.Unix() || fc.searchReq.GetUntil() != day.Add(24*time.Hour-time.Second).Unix() {
		t.Fatalf("range = %d..%d", fc.searchReq.GetSince(), fc.searchReq.GetUntil())
	}
	out := resultText(res)
	for _, want := range []string{`"session://a/transcript" ("session_transcript") · 2026-10-21 14:02 to 15:02 UTC`, `"session://b/record" ("session_record") · date unknown`, `"repo://x/README.md" ("repo_doc") · unverified`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if res, _ := rt.search(context.Background(), toolReq(map[string]any{"query": "q", "since": "last week"})); !res.IsError {
		t.Fatal("a bad since was accepted")
	}
	if res, _ := rt.search(context.Background(), toolReq(map[string]any{"query": "q", "until": "soon"})); !res.IsError {
		t.Fatal("a bad until was accepted")
	}
}

func TestListSourcesDatesAndRanges(t *testing.T) {
	fc := &fakeClient{listSources: &orchdv1.ListSourcesResponse{TotalSources: 2, TotalChunks: 3, Sources: []*orchdv1.SourceSummary{
		{SourceUri: "session://a/record", SourceType: "session_record", Chunks: 1, StartedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC).Unix()},
		{SourceUri: "session://b/record", SourceType: "session_record", Chunks: 2},
	}}}
	rt := ragTools{client: fc}
	res, err := rt.listSources(context.Background(), toolReq(map[string]any{"since": "7d"}))
	if err != nil || res.IsError || fc.listReq.GetSince() == 0 || fc.listReq.GetUntil() != 0 {
		t.Fatalf("list = %v, req %+v, %v", resultText(res), fc.listReq, err)
	}
	out := resultText(res)
	if !strings.Contains(out, "when=2026-10-20 09:00 UTC") || !strings.Contains(out, "when=date unknown") {
		t.Fatalf("output:\n%s", out)
	}
	if res, _ := rt.listSources(context.Background(), toolReq(map[string]any{"until": "x"})); !res.IsError {
		t.Fatal("a bad until was accepted")
	}
}
