package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config/configtest"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
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
	if err != nil || !strings.Contains(out, `("session_transcript") · 2026-10-21 14:02 UTC`) || fc.searchReq.GetSince() == 0 || fc.searchReq.GetUntil() <= fc.searchReq.GetSince() {
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
	t.Setenv(agentrun.TaskIDEnv, "")
	var got []*orchdv1.BackfillSessionsRequest
	old := fakeBackfill
	t.Cleanup(func() { fakeBackfill = old })
	fakeBackfill = func(req *orchdv1.BackfillSessionsRequest) (*orchdv1.BackfillSessionsResponse, error) {
		got = append(got, req)
		return &orchdv1.BackfillSessionsResponse{Total: 4, Todo: 1, Dated: 1, Missing: 2, Handed: 1}, nil
	}
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "rag", "backfill-sessions")
	if err != nil || !strings.Contains(out, "4 session(s) in memory: 1 to store again with dates, 1 already dated, 2 without a transcript on disk") ||
		!strings.Contains(out, "plan only") || got[0].GetApply() {
		t.Fatalf("plan: %q, %v", out, err)
	}
	out, err = run(cf, "", "rag", "backfill-sessions", "--apply")
	if err != nil || !strings.Contains(out, "handed 1 session(s) to orchd") || !got[1].GetApply() || got[1].GetAll() {
		t.Fatalf("apply: %q, %v", out, err)
	}
	out, err = run(cf, "", "rag", "backfill-sessions", "--all", "--summaries")
	if err != nil || !strings.Contains(out, "1 transcript(s) to store again") || !strings.Contains(out, "up to 1 model call") ||
		!got[2].GetAll() || !got[2].GetSummaries() || got[2].GetApply() {
		t.Fatalf("all: %q, %v", out, err)
	}
	if _, err := run(cf, "", "rag", "backfill-sessions", "--summaries"); err == nil {
		t.Fatal("--summaries without --all")
	}
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	if _, err := run(cf, "", "rag", "backfill-sessions", "--apply"); err == nil {
		t.Fatal("a queue task backfilled")
	}
}

func TestRAGWhy(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Run("a follow-up is not searched", func(t *testing.T) {
		fc := &fakeClient{}
		out, err := run(factory(fc, nil), "", "rag", "why", "Let's merge it.")
		if err != nil || !strings.Contains(out, "no search: names no topic") || !strings.Contains(out, "topic terms: none") {
			t.Fatalf("%q, %v", out, err)
		}
	})
	hits := &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop backs off", Score: 0.5},
		{SourceUri: "repo://a/noise.md", SourceType: "repo_doc", Text: "kiln firing", Score: 0.8},
	}}
	t.Run("candidates and the block", func(t *testing.T) {
		out, err := run(factory(&fakeClient{search: hits}, nil), "", "rag", "why", "fix the socket reconnect loop", "--cwd", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"repo://a/one.md"  unverified  kept`, `"repo://a/noise.md"  unverified  dropped: shares too little`, "injected: 1 pointer(s)", "## Auto-injected RAG context"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
	})
	t.Run("nothing qualifies", func(t *testing.T) {
		out, err := run(factory(&fakeClient{search: &orchdv1.SearchResponse{}}, nil), "", "rag", "why", "fix the socket reconnect loop")
		if err != nil || !strings.Contains(out, "injected: nothing") {
			t.Fatalf("%q, %v", out, err)
		}
	})
	t.Run("a failed search says so", func(t *testing.T) {
		out, err := run(factory(&fakeClient{err: errors.New("qdrant is down")}, nil), "", "rag", "why", "fix the socket reconnect loop")
		if err != nil || !strings.Contains(out, "search failed: qdrant is down") {
			t.Fatalf("%q, %v", out, err)
		}
	})
	t.Run("json", func(t *testing.T) {
		out, err := run(factory(&fakeClient{search: hits}, nil), "", "rag", "why", "fix the socket reconnect loop", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Gated      bool `json:"gated"`
			Candidates []struct {
				URI    string `json:"uri"`
				Kept   bool   `json:"kept"`
				Reason string `json:"reason"`
			} `json:"candidates"`
			Block string `json:"block"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if !v.Gated || len(v.Candidates) < 2 || !v.Candidates[0].Kept || v.Candidates[1].Reason == "" || v.Block == "" {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("reads the prompt from stdin", func(t *testing.T) {
		if got, err := whyPrompt([]string{"-"}, strings.NewReader("  a pasted log\n")); err != nil || got != "a pasted log" {
			t.Fatalf("%q, %v", got, err)
		}
		if got, err := whyPrompt(nil, strings.NewReader("from stdin")); err != nil || got != "from stdin" {
			t.Fatalf("%q, %v", got, err)
		}
		if got, err := whyPrompt([]string{" an arg "}, nil); err != nil || got != "an arg" {
			t.Fatalf("%q, %v", got, err)
		}
		if _, err := whyPrompt(nil, strings.NewReader("  ")); err == nil {
			t.Fatal("empty stdin")
		}
		if _, err := whyPrompt(nil, iotest.ErrReader(errors.New("broken"))); err == nil {
			t.Fatal("failed read")
		}
	})
	t.Run("gate off and disabled are stated", func(t *testing.T) {
		t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
		configtest.Set(t, map[string]string{"hooks.auto_rag.enabled": "false", "hooks.auto_rag.gate": `"off"`})
		out, err := run(factory(&fakeClient{search: &orchdv1.SearchResponse{}}, nil), "", "rag", "why", "Let's merge it right now.")
		if err != nil || !strings.Contains(out, "gate: off") || !strings.Contains(out, "enabled is off") {
			t.Fatalf("%q, %v", out, err)
		}
	})
}

func TestTermListCaps(t *testing.T) {
	var terms []string
	for i := range 15 {
		terms = append(terms, fmt.Sprintf("t%02d", i))
	}
	if got := termList(terms); !strings.HasSuffix(got, "+3 more") {
		t.Fatal(got)
	}
}
