package cli

import (
	"encoding/json"
	"errors"
	"fmt"
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
	if err := handoff.DoneSession(handoff.Dir(), left[0]); err != nil {
		t.Fatal(err)
	}
	// --all takes every stored transcript, dated or not, and leaves the records
	// alone. A session whose transcript was forgotten stays forgotten, and one
	// already waiting on orchd keeps what it asked for.
	if err := handoff.PutSession(handoff.Dir(), handoff.Session{ID: "pending", TranscriptPath: tr, Record: true, Summarize: true}); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "pending", TranscriptPath: tr}); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "forgotten", TranscriptPath: tr}); err != nil {
		t.Fatal(err)
	}
	fc.listSources.Sources = append(fc.listSources.Sources,
		&orchdv1.SourceSummary{SourceUri: "session://pending/transcript"}, &orchdv1.SourceSummary{SourceUri: "session://forgotten/record"})
	out, err = run(cf, "", "rag", "backfill-sessions", "--all", "--apply")
	if err != nil || !strings.Contains(out, "6 session(s) in memory: 3 transcript(s) to store again, 0 without a transcript on disk") || !strings.Contains(out, "handed 2 session(s)") {
		t.Fatalf("all: %q, %v", out, err)
	}
	left, _ = handoff.Sessions(handoff.Dir(), time.Now().Add(time.Hour))
	got := map[string]handoff.Session{}
	for _, s := range left {
		got[s.ID] = s
	}
	if len(left) != 3 || got["todo"].Record || got["dated"].Record || !got["pending"].Record || !got["pending"].Summarize {
		t.Fatalf("--all handed off %+v", left)
	}
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	if _, err := run(cf, "", "rag", "backfill-sessions", "--apply"); err == nil {
		t.Fatal("a queue task backfilled")
	}
}

func TestRAGWhy(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
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
	t.Run("gate off and disabled are stated", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ORCH_CONFIG_DIR", dir)
		cfg := "hooks:\n  auto_rag:\n    enabled: false\n    gate: off\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
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
