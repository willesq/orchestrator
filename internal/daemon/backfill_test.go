package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

type backfillRag struct {
	ragService
	sources []rag.SourceSummary
}

func (r backfillRag) ListSources(context.Context, rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error) {
	return r.sources, len(r.sources), 0, nil
}

func TestBackfillSessions(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	tr := func(id string) string {
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, r := range []*session.Record{
		{SessionID: "todo", TranscriptPath: tr("todo")},
		{SessionID: "gone", TranscriptPath: "/nonexistent/gone.jsonl"},
		{SessionID: "dated", TranscriptPath: tr("dated")},
		{SessionID: "forgotten", TranscriptPath: tr("forgotten")},
		{SessionID: "pending", TranscriptPath: tr("pending")},
		{SessionID: "swapped", TranscriptPath: tr("todo")}, // names another session's file
	} {
		if err := session.WriteRecord(paths.SessionsDir(), r); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{rag: backfillRag{sources: []rag.SourceSummary{
		{SourceURI: "session://todo/transcript", Tags: []string{"other"}},
		{SourceURI: "session://todo/record", Tags: []string{"checkout:/x"}},
		{SourceURI: "session://gone/transcript"},
		{SourceURI: "session://norecord/record"},
		{SourceURI: "session://dated/transcript", StartedAt: time.Unix(100, 0)},
		{SourceURI: "session://forgotten/record"},
		{SourceURI: "session://pending/transcript", StartedAt: time.Unix(100, 0)},
		{SourceURI: "session://swapped/transcript"},
		{SourceURI: "session://../evil/record"},
		{SourceURI: "manual://x"},
	}}}
	ctx := context.Background()
	pending := func() map[string]handoff.Session {
		t.Helper()
		left, err := handoff.Sessions(handoff.Dir(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]handoff.Session{}
		for _, p := range left {
			m[p.ID] = p
		}
		return m
	}

	// A plan hands nothing off.
	plan, err := s.BackfillSessions(ctx, &orchdv1.BackfillSessionsRequest{})
	if err != nil || plan.GetTotal() != 7 || plan.GetTodo() != 1+1 || plan.GetDated() != 2 || plan.GetMissing() != 3 {
		// todo and forgotten have no dates and a file; gone, norecord and swapped do not.
		t.Fatalf("plan: %+v, %v", plan, err)
	}
	if len(pending()) != 0 {
		t.Fatal("a plan handed sessions off")
	}

	// Plain backfill stores the undated, with their records.
	if _, err := s.BackfillSessions(ctx, &orchdv1.BackfillSessionsRequest{Apply: true}); err != nil {
		t.Fatal(err)
	}
	got := pending()
	if !got["todo"].Record || got["todo"].Summarize || got["todo"].TranscriptPath != tr("todo") || len(got["todo"].Tags) != 1 || got["todo"].Tags[0] != "checkout:/x" {
		t.Fatalf("handed off %+v", got)
	}
	for _, p := range got {
		if err := handoff.DoneSession(handoff.Dir(), p); err != nil {
			t.Fatal(err)
		}
	}

	// --all takes every stored transcript, dated or not, and leaves records alone.
	// A forgotten transcript stays forgotten; one already waiting keeps its asks.
	if err := handoff.PutSession(handoff.Dir(), handoff.Session{ID: "pending", TranscriptPath: tr("pending"), Record: true, Summarize: true}); err != nil {
		t.Fatal(err)
	}
	r, err := s.BackfillSessions(ctx, &orchdv1.BackfillSessionsRequest{All: true, Summaries: true, Apply: true})
	if err != nil || r.GetTodo() != 3 || r.GetHanded() != 2 || r.GetWaiting() != 1 || r.GetMissing() != 2 {
		t.Fatalf("all: %+v, %v", r, err)
	}
	got = pending()
	if len(got) != 3 || got["todo"].Record || !got["todo"].Summarize || got["dated"].Record || !got["pending"].Record {
		t.Fatalf("--all handed off %+v", got)
	}
	if _, ok := got["forgotten"]; ok {
		t.Fatal("brought back a forgotten transcript")
	}
}

func TestBackfillIsRefusedToQueuedTasks(t *testing.T) {
	if taskCallable[orchdv1.Orchd_BackfillSessions_FullMethodName] || operatorActions[orchdv1.Orchd_BackfillSessions_FullMethodName] == "" {
		t.Fatal("BackfillSessions must be operator-only")
	}
}
