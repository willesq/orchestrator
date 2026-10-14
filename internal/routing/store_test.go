package routing

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func table(t *testing.T, s *Store) string {
	t.Helper()
	rs, err := s.Table(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return refs(rs)
}

func route(kind string, rank int, model string) Route {
	return Route{Kind: kind, Rank: rank, Provider: "claude-code", Model: model, Rationale: "r", DecidedAt: 1}
}

func TestStoreApplyAndRollback(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()
	if err := s.Rollback(ctx); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("rollback with no history err = %v", err)
	}
	first := []Route{route("code", 1, "opus"), route("code", 2, "sonnet"), route("docs", 1, "sonnet")}
	if _, err := s.Apply(ctx, first, 1); err != nil {
		t.Fatal(err)
	}
	if got := table(t, s); got != "claude-code/opus,claude-code/sonnet,claude-code/sonnet" {
		t.Fatalf("after apply: %s", got)
	}
	second := []Route{route("code", 1, "haiku")}
	if _, err := s.Apply(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	if got := table(t, s); got != "claude-code/haiku" {
		t.Fatalf("after second apply: %s", got)
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := table(t, s); got != "claude-code/opus,claude-code/sonnet,claude-code/sonnet" {
		t.Fatalf("after rollback: %s", got)
	}
	// Rolling back past the first apply restores the empty table.
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := table(t, s); got != "" {
		t.Fatalf("after second rollback: %q", got)
	}
	if err := s.Rollback(ctx); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("history should be exhausted: %v", err)
	}
}

func TestStorePinSurvivesApply(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()
	if _, err := s.Apply(ctx, []Route{route("code", 1, "opus"), route("docs", 1, "sonnet")}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, Route{Kind: "code", Provider: "codex", Model: "gpt-sol", Effort: "high"}, 2); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.Apply(ctx, []Route{route("code", 1, "haiku"), route("code", 2, "sonnet"), route("docs", 1, "haiku")}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if pinned != 1 {
		t.Fatalf("pinned kinds = %d", pinned)
	}
	rs, err := s.Kind(ctx, "code")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Ref() != "codex/gpt-sol@high" || !rs[0].Pinned || rs[0].Rationale != "set by hand" {
		t.Fatalf("pinned kind changed: %+v", rs)
	}
	if got := table(t, s); got != "codex/gpt-sol@high,claude-code/haiku" {
		t.Fatalf("table = %s", got)
	}
	if err := s.Clear(ctx, "code", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, []Route{route("code", 1, "haiku")}, 5); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Kind(ctx, "code"); len(rs) != 1 || rs[0].Model != "haiku" || rs[0].Pinned {
		t.Fatalf("cleared kind not refilled: %+v", rs)
	}
	// Each change was recorded: rollback undoes apply, then clear.
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Kind(ctx, "code"); len(rs) != 1 || !rs[0].Pinned {
		t.Fatalf("rollback did not restore the pin: %+v", rs)
	}
}

func TestStoreRejectsUnknownKind(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()
	if err := s.Set(ctx, Route{Kind: "poetry", Provider: "claude-code", Model: "opus"}, 1); err == nil {
		t.Fatal("set accepted an unknown kind")
	}
	if err := s.Clear(ctx, "poetry", 1); err == nil {
		t.Fatal("clear accepted an unknown kind")
	}
}

func TestStoreReportsDatabaseErrors(t *testing.T) {
	conn := testDB(t)
	s := NewStore(conn)
	_ = conn.Close()
	ctx := context.Background()
	r := Route{Kind: "code", Provider: "claude-code", Model: "opus"}
	checks := map[string]error{
		"table":    func() error { _, err := s.Table(ctx); return err }(),
		"kind":     func() error { _, err := s.Kind(ctx, "code"); return err }(),
		"apply":    func() error { _, err := s.Apply(ctx, []Route{r}, 1); return err }(),
		"set":      s.Set(ctx, r, 1),
		"clear":    s.Clear(ctx, "code", 1),
		"rollback": s.Rollback(ctx),
	}
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s on a closed database returned no error", name)
		}
	}
	rt := Router{Store: s, Models: models.NewRegistry(conn), Policy: testPolicy()}
	if _, err := rt.Recommend(ctx, "code", nil); err == nil {
		t.Error("recommend on a closed database returned no error")
	}
	if _, err := rt.Pin(ctx, "claude-code/opus", nil); err == nil {
		t.Error("pin on a closed database returned no error")
	}
}

func TestStoreApplyFailsOnDuplicateRank(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()
	if _, err := s.Apply(ctx, []Route{route("code", 1, "opus")}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, []Route{route("code", 1, "opus"), route("code", 1, "sonnet")}, 2); err == nil {
		t.Fatal("duplicate rank accepted")
	}
	// The failed apply left the table and history untouched.
	if got := table(t, s); got != "claude-code/opus" {
		t.Fatalf("table = %s", got)
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := table(t, s); got != "" {
		t.Fatalf("after rollback = %q", got)
	}
}

func TestStoreKeepsBoundedHistory(t *testing.T) {
	conn := testDB(t)
	s := NewStore(conn)
	ctx := context.Background()
	for i := 0; i < historyKeep+15; i++ {
		if err := s.Set(ctx, Route{Kind: "code", Provider: "claude-code", Model: "opus"}, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	var gens int
	if err := conn.QueryRow(`SELECT COUNT(DISTINCT generation) FROM routing_history`).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if gens > historyKeep {
		t.Fatalf("history holds %d generations, want at most %d", gens, historyKeep)
	}
	for i := 0; i < gens; i++ {
		if err := s.Rollback(ctx); err != nil {
			t.Fatalf("rollback %d: %v", i, err)
		}
	}
	if err := s.Rollback(ctx); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("err = %v", err)
	}
}
