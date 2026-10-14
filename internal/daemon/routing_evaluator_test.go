package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/paths"
)

func evaluatorSettings(enabled bool, ranks int) config.Settings {
	var s config.Settings
	s.Routing.Evaluate.Enabled = enabled
	s.Routing.Evaluate.Interval = "1h"
	s.Routing.Ranks = ranks
	s.Routing.MinEvidence = 5
	s.Routing.Providers = []string{"claude-code"}
	return s
}

func TestRoutingEvaluatorUnit(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, ok := routingEvaluatorUnit(evaluatorSettings(false, 3), conn); ok {
		t.Error("disabled evaluator was built")
	}
	if _, ok := routingEvaluatorUnit(evaluatorSettings(true, 3), nil); ok {
		t.Error("evaluator built without a database")
	}
	if _, ok := routingEvaluatorUnit(evaluatorSettings(true, 0), conn); ok {
		t.Error("evaluator built with an invalid policy")
	}
	u, ok := routingEvaluatorUnit(evaluatorSettings(true, 3), conn)
	if !ok || u.Name != UnitRoutingEvaluator || !strings.Contains(u.Detail, "proposes") {
		t.Fatalf("unit = %+v", u)
	}
	auto := evaluatorSettings(true, 3)
	auto.Routing.AutoApply = true
	if u, _ := routingEvaluatorUnit(auto, conn); !strings.Contains(u.Detail, "applies") {
		t.Fatalf("auto-apply detail = %q", u.Detail)
	}

	// A tick discovers (no CLIs on PATH here, so providers read unavailable)
	// and records its run without failing.
	orig := models.LookPath
	t.Cleanup(func() { models.LookPath = orig })
	models.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	u.Tick(context.Background())
	if _, err := os.Stat(paths.RoutingEvaluatorStateFile()); err != nil {
		t.Fatalf("no state recorded: %v", err)
	}
	ps, _ := models.NewRegistry(conn).Providers(context.Background())
	if len(ps) != 2 || ps[0].State != models.StateUnavailable {
		t.Fatalf("providers = %+v", ps)
	}
}

func TestDiscovererReportsOnlyNewDiscoveredModels(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	orig := models.LookPath
	t.Cleanup(func() { models.LookPath = orig })
	models.LookPath = func(name string) (string, error) { return "/bin/" + filepath.Base(name), nil }
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"models":[{"slug":"gpt-a","visibility":"list"}]}`
	if err := os.WriteFile(filepath.Join(home, ".codex", "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	discover := discoverer(conn, "/opt/claude")
	fresh, err := discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The compiled-in Claude aliases start enabled, so only the cache model is "new".
	if strings.Join(fresh, ",") != "codex/gpt-a" {
		t.Fatalf("fresh = %v", fresh)
	}
	if fresh, _ = discover(context.Background()); len(fresh) != 0 {
		t.Fatalf("second run fresh = %v", fresh)
	}
}

func TestEmitRouting(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := emitRouting("2 routing change(s) proposed"); err != nil {
		t.Fatal(err)
	}
	evs, err := events.Read(paths.EventsDir(), routingEventStream)
	if err != nil || len(evs) != 1 || evs[0].Source != UnitRoutingEvaluator || evs[0].Kind != "info" {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}
