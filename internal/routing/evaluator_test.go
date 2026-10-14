package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type evalHarness struct {
	ev      *Evaluator
	emitted []string
	fresh   []string
	refErr  error
	emitErr error
	now     time.Time
}

func newEvalHarness(t *testing.T) *evalHarness {
	t.Helper()
	h := &evalHarness{now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	h.ev = &Evaluator{
		Router:    testRouter(t),
		Refresh:   func(context.Context) ([]string, error) { return h.fresh, h.refErr },
		Emit:      func(s string) error { h.emitted = append(h.emitted, s); return h.emitErr },
		Every:     7 * 24 * time.Hour,
		StatePath: filepath.Join(t.TempDir(), "state", "routing.json"),
		Now:       func() time.Time { return h.now },
	}
	return h
}

func TestEvaluatorProposesOnCadence(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	h.fresh = []string{"codex/gpt-new"}
	if err := h.ev.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.emitted) != 1 || !strings.Contains(h.emitted[0], "1 new model(s) discovered (codex/gpt-new)") ||
		!strings.Contains(h.emitted[0], "routing change(s) proposed") {
		t.Fatalf("emitted = %q", h.emitted)
	}
	if tbl, _ := h.ev.Router.Store.Table(ctx); len(tbl) != 0 {
		t.Fatal("proposing must not apply")
	}
	// Not due yet: nothing runs.
	h.fresh = nil
	h.now = h.now.Add(24 * time.Hour)
	calls := 0
	h.ev.Refresh = func(context.Context) ([]string, error) { calls++; return nil, nil }
	if err := h.ev.Tick(ctx); err != nil || calls != 0 {
		t.Fatalf("early tick ran: calls=%d err=%v", calls, err)
	}
	// Due again with the same proposal: run, but do not re-announce it.
	h.now = h.now.Add(7 * 24 * time.Hour)
	h.ev.Refresh = func(context.Context) ([]string, error) { calls++; return nil, nil }
	if err := h.ev.Tick(ctx); err != nil || calls != 1 {
		t.Fatalf("due tick: calls=%d err=%v", calls, err)
	}
	if len(h.emitted) != 2 {
		t.Fatalf("emitted = %q; the proposal changed (no new models), so it is reported once", h.emitted)
	}
	h.now = h.now.Add(7 * 24 * time.Hour)
	_ = h.ev.Tick(ctx)
	if len(h.emitted) != 2 {
		t.Fatalf("unchanged proposal re-announced: %q", h.emitted)
	}
}

func TestEvaluatorStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	if err := h.ev.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A new evaluator over the same state file (orchd restarted) waits out the cadence.
	again := *h.ev
	ran := false
	again.Refresh = func(context.Context) ([]string, error) { ran = true; return nil, nil }
	h.now = h.now.Add(time.Hour)
	if err := again.Tick(ctx); err != nil || ran {
		t.Fatalf("restart re-ran early: ran=%v err=%v", ran, err)
	}
	if _, err := os.Stat(h.ev.StatePath); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluatorAutoApply(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	h.ev.AutoApply = true
	if err := h.ev.Router.Store.Set(ctx, Route{Kind: "docs", Provider: "codex", Model: "gpt-sol"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.ev.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.emitted) != 1 || !strings.Contains(h.emitted[0], "applied") || !strings.Contains(h.emitted[0], "rollback") {
		t.Fatalf("emitted = %q", h.emitted)
	}
	docs, _ := h.ev.Router.Store.Kind(ctx, "docs")
	if len(docs) != 1 || !docs[0].Pinned || docs[0].Model != "gpt-sol" {
		t.Fatalf("pinned kind changed: %+v", docs)
	}
	code, _ := h.ev.Router.Store.Kind(ctx, "code")
	if len(code) == 0 {
		t.Fatal("auto-apply wrote nothing")
	}
	// The pinned kind is not counted as a pending change afterwards.
	h.now = h.now.Add(8 * 24 * time.Hour)
	h.emitted = nil
	if err := h.ev.Tick(ctx); err != nil || len(h.emitted) != 0 {
		t.Fatalf("nothing left to change, yet emitted %q (%v)", h.emitted, err)
	}
}

func TestEvaluatorErrors(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	h.refErr = errors.New("codex: parse failed")
	err := h.ev.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("err = %v", err)
	}
	if len(h.emitted) != 1 {
		t.Fatal("a discovery failure must still evaluate")
	}

	h2 := newEvalHarness(t)
	h2.emitErr = errors.New("disk full")
	if err := h2.ev.Tick(ctx); err == nil {
		t.Fatal("emit failure not reported")
	}
	// The report was not delivered, so the next due run tries it again.
	h2.emitErr = nil
	h2.now = h2.now.Add(8 * 24 * time.Hour)
	_ = h2.ev.Tick(ctx)
	if len(h2.emitted) != 2 {
		t.Fatalf("undelivered report not retried: %q", h2.emitted)
	}

	h3 := newEvalHarness(t)
	_ = h3.ev.Router.Store.db.Close()
	if err := h3.ev.Tick(ctx); err == nil {
		t.Fatal("closed database: want error")
	}
	if st := h3.ev.load(); st.LastRun != 0 || st.LastAttempt == 0 {
		t.Fatalf("a failed run must be recorded as attempted, not done: %+v", st)
	}
}

func TestFirstN(t *testing.T) {
	if got := strings.Join(firstN([]string{"a", "b", "c"}, 2), ","); got != "a,b,+1 more" {
		t.Fatalf("firstN = %q", got)
	}
	if got := len(firstN([]string{"a"}, 2)); got != 1 {
		t.Fatalf("firstN short = %d", got)
	}
}

func TestEvaluatorHoldsRiskyAutoApply(t *testing.T) {
	ctx := context.Background()
	t.Run("discovery failed", func(t *testing.T) {
		h := newEvalHarness(t)
		h.ev.AutoApply = true
		h.refErr = errors.New("claude-code: gone")
		_ = h.ev.Tick(ctx)
		if tbl, _ := h.ev.Router.Store.Table(ctx); len(tbl) != 0 {
			t.Fatal("applied after a failed discovery")
		}
		if len(h.emitted) != 1 || !strings.Contains(h.emitted[0], "not auto-applied: discovery failed") {
			t.Fatalf("emitted = %q", h.emitted)
		}
	})
	t.Run("removes routes", func(t *testing.T) {
		h := newEvalHarness(t)
		h.ev.AutoApply = true
		if _, err := h.ev.Router.Store.Apply(ctx, []Route{
			{Kind: "code", Rank: 1, Provider: "claude-code", Model: "opus", DecidedAt: 1},
			{Kind: "code", Rank: 2, Provider: "claude-code", Model: "sonnet", DecidedAt: 1},
			{Kind: "code", Rank: 3, Provider: "codex", Model: "gpt-sol", DecidedAt: 1},
			{Kind: "code", Rank: 4, Provider: "claude-code", Model: "sonnet", DecidedAt: 1},
		}, 1); err != nil {
			t.Fatal(err)
		}
		_ = h.ev.Tick(ctx)
		if code, _ := h.ev.Router.Store.Kind(ctx, "code"); len(code) != 4 {
			t.Fatalf("a removal was auto-applied: %+v", code)
		}
		if len(h.emitted) != 1 || !strings.Contains(h.emitted[0], "not auto-applied: it removes routes") ||
			!strings.Contains(h.emitted[0], "code#4 claude-code/sonnet→none") {
			t.Fatalf("emitted = %q", h.emitted)
		}
	})
}

func TestEvaluatorNamesAppliedChangesAndAlwaysReportsThem(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	h.ev.AutoApply = true
	if err := h.ev.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.emitted) != 1 || !strings.Contains(h.emitted[0], "code#1 none→claude-code/opus") {
		t.Fatalf("emitted = %q", h.emitted)
	}
	// Undo it and pre-seed the report hash, as a tampered state file would: the
	// next apply must still be announced.
	if err := h.ev.Router.Store.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	st := h.ev.load()
	st.LastReport = digest(h.emitted[0])
	if err := h.ev.save(st); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(8 * 24 * time.Hour)
	_ = h.ev.Tick(ctx)
	if len(h.emitted) != 2 {
		t.Fatalf("a repeated apply went unannounced: %q", h.emitted)
	}
}

func TestEvaluatorStateEdgeCases(t *testing.T) {
	ctx := context.Background()
	h := newEvalHarness(t)
	if err := h.ev.save(evaluatorState{LastRun: h.now.Add(1000 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := h.ev.Tick(ctx); err != nil || len(h.emitted) != 1 {
		t.Fatalf("a future last_run suppressed the run: %q %v", h.emitted, err)
	}

	// A failed run keeps what it discovered and backs off before retrying.
	h2 := newEvalHarness(t)
	h2.fresh = []string{"codex/gpt-new"}
	db := h2.ev.Router.Store.db
	_ = db.Close()
	if err := h2.ev.Tick(ctx); err == nil {
		t.Fatal("want error")
	}
	if st := h2.ev.load(); strings.Join(st.Pending, ",") != "codex/gpt-new" {
		t.Fatalf("pending = %v", st.Pending)
	}
	calls := 0
	h2.ev.Refresh = func(context.Context) ([]string, error) { calls++; return nil, nil }
	h2.now = h2.now.Add(time.Hour)
	_ = h2.ev.Tick(ctx)
	if calls != 0 {
		t.Fatal("retried inside the backoff")
	}
	// Once healthy again, the held discovery is reported.
	h2.ev.Router = testRouter(t)
	h2.now = h2.now.Add(retryAfter)
	if err := h2.ev.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(h2.emitted) != 1 || !strings.Contains(h2.emitted[0], "codex/gpt-new") {
		t.Fatalf("calls=%d emitted=%q", calls, h2.emitted)
	}
	if st := h2.ev.load(); len(st.Pending) != 0 {
		t.Fatalf("pending not cleared: %v", st.Pending)
	}
}

func TestUnion(t *testing.T) {
	if got := strings.Join(union([]string{"a", "b"}, []string{"b", "c"}), ","); got != "a,b,c" {
		t.Fatalf("union = %q", got)
	}
}
