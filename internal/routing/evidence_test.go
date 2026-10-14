package routing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/models"
)

// addTask records a finished routed task with the given labels.
func addTask(t *testing.T, conn *sql.DB, kind, provider, model, status, rating, prOutcome string) {
	t.Helper()
	var n int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n)
	_, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, rating, pr_outcome)
		VALUES (?, 'dev', 'b', '', ?, datetime('now'), ?, ?, ?, ?, ?)`,
		fmt.Sprintf("task-%d", n), status, kind, provider, model, rating, prOutcome)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceTally(t *testing.T) {
	conn := testDB(t)
	addTask(t, conn, "code", "claude-code", "opus", "complete", "good", "bad") // rating beats PR outcome
	addTask(t, conn, "code", "claude-code", "opus", "complete", "", "rework")
	addTask(t, conn, "code", "claude-code", "opus", "failed", "", "")
	addTask(t, conn, "code", "claude-code", "opus", "complete", "", "") // no verdict: not counted
	addTask(t, conn, "code", "claude-code", "opus", "failed", "rework", "")
	addTask(t, conn, "docs", "claude-code", "opus", "complete", "bad", "")
	addTask(t, conn, "", "claude-code", "opus", "complete", "good", "") // unrouted: not counted
	ev, err := NewStore(conn).Evidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code := ev.For("code", "claude-code", "opus")
	if code != (Tally{Good: 1, Rework: 2, Bad: 1}) {
		t.Fatalf("code tally = %+v", code)
	}
	if got := code.Score(); got != 0.5 {
		t.Fatalf("score = %v", got)
	}
	if ev.For("docs", "claude-code", "opus") != (Tally{Bad: 1}) {
		t.Fatalf("docs tally = %+v", ev.For("docs", "claude-code", "opus"))
	}
	if (Tally{}).Score() != 0 || ev.For("code", "codex", "x").N() != 0 {
		t.Fatal("empty tally should score 0")
	}
}

func TestEvidenceMovesRanks(t *testing.T) {
	cands := []models.Model{
		enabled("claude-code", "opus", models.TierFrontier, "alias"),
		enabled("claude-code", "sonnet", models.TierBalanced, "alias"),
	}
	p := testPolicy()
	tests := []struct {
		name     string
		ev       Evidence
		wantTop  string
		wantWhy  string
		wantTopN int
	}{
		{name: "no evidence keeps the prior", wantTop: "opus", wantWhy: "no outcome evidence yet"},
		{name: "below the minimum keeps the prior",
			ev:      Evidence{evidenceKey("code", "claude-code", "opus"): {Bad: 4}},
			wantTop: "opus", wantWhy: "below the 5 needed", wantTopN: 4},
		{name: "enough failures demote the frontier model",
			ev: Evidence{
				evidenceKey("code", "claude-code", "opus"):   {Bad: 9, Good: 1},
				evidenceKey("code", "claude-code", "sonnet"): {Good: 10},
			},
			wantTop: "sonnet", wantWhy: "observed 10 good, 0 rework, 0 bad (100% success)", wantTopN: 10},
		{name: "a good record holds the frontier model",
			ev:      Evidence{evidenceKey("code", "claude-code", "opus"): {Good: 6, Rework: 2}},
			wantTop: "opus", wantWhy: "observed 6 good, 2 rework", wantTopN: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proposeKind(p, "code", cands, tt.ev, 1)
			if got[0].Model != tt.wantTop || got[0].EvidenceN != tt.wantTopN {
				t.Fatalf("top = %+v", got[0])
			}
			if !strings.Contains(got[0].Rationale, tt.wantWhy) {
				t.Fatalf("rationale = %q, want %q", got[0].Rationale, tt.wantWhy)
			}
		})
	}
}

func TestScoreWeighting(t *testing.T) {
	p := testPolicy()
	m := enabled("claude-code", "opus", models.TierFrontier, "alias")
	if got := score(p, models.TierFrontier, m, Tally{Bad: 4}); got != 1 {
		t.Fatalf("below minimum = %v, want the prior", got)
	}
	few := score(p, models.TierFrontier, m, Tally{Bad: 5})
	many := score(p, models.TierFrontier, m, Tally{Bad: 50})
	if !(few < 1 && many < few && many > 0) {
		t.Fatalf("weights: few=%v many=%v", few, many)
	}
}

func TestRecommendReportsLiveEvidence(t *testing.T) {
	ctx := context.Background()
	r := testRouter(t)
	if err := r.Store.Set(ctx, Route{Kind: "code", Provider: "claude-code", Model: "opus"}, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		addTask(t, r.Store.db, "code", "claude-code", "opus", "complete", "good", "")
	}
	rec, err := r.Recommend(ctx, "code", nil)
	if err != nil || rec.EvidenceN != 6 || rec.Confidence != "medium" {
		t.Fatalf("rec = %+v, %v", rec, err)
	}
}

func TestLiveFallbackIgnoresEvidence(t *testing.T) {
	ctx := context.Background()
	r := testRouter(t)
	for i := 0; i < 20; i++ {
		addTask(t, r.Store.db, "code", "claude-code", "opus", "complete", "bad", "")
		addTask(t, r.Store.db, "code", "claude-code", "sonnet", "complete", "good", "")
	}
	rec, err := r.Recommend(ctx, "code", onlyClaude)
	if err != nil {
		t.Fatal(err)
	}
	// Evidence reaches dispatch only through an applied table.
	if rec.Model != "opus" || rec.FromTable {
		t.Fatalf("fallback = %+v, want the declared-capability choice", rec)
	}
	all, _ := r.Candidates(ctx)
	var cands []models.Model
	for _, m := range all {
		if onlyClaude(m.Provider) {
			cands = append(cands, m)
		}
	}
	ev, _ := r.Store.Evidence(ctx)
	if top := proposeKind(r.Policy, "code", cands, ev, 1)[0]; top.Model != "sonnet" {
		t.Fatalf("proposal top = %+v, want evidence to move it", top)
	}
}
