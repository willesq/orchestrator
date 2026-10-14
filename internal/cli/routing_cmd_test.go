package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// seedModels registers claude-code (opus, sonnet, haiku) and codex (gpt-sol,
// enabled, with efforts) in a fresh orch.db.
func seedModels(t *testing.T) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv(agentrun.TaskIDEnv, "")
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	reg := models.NewRegistry(conn)
	ctx := context.Background()
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "claude-code", State: models.StateEnabled}, models.StateEnabled, []models.Model{
		{Slug: "opus", Tier: models.TierFrontier, Source: "alias", State: models.StateEnabled},
		{Slug: "sonnet", Tier: models.TierBalanced, Source: "alias", State: models.StateEnabled},
		{Slug: "haiku", Tier: models.TierFast, Source: "alias", State: models.StateEnabled},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "codex", State: models.StateEnabled}, models.StateEnabled, []models.Model{
		{Slug: "gpt-sol", Source: "cache", SupportedEfforts: []string{"low", "high"}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetModelState(ctx, "codex", "gpt-sol", models.StateEnabled); err != nil {
		t.Fatal(err)
	}
}

func TestRoutingCommands(t *testing.T) {
	seedModels(t)
	steps := []struct {
		args    []string
		want    []string
		not     []string
		wantErr string
	}{
		{args: []string{"routing", "show"}, want: []string{"routing table is empty"}},
		{args: []string{"routing", "rollback"}, wantErr: "no earlier routing table"},
		{args: []string{"routing", "evaluate"}, want: []string{"+ code #1 claude-code/opus", "+ classify #1 claude-code/haiku", "change(s) proposed"}},
		{args: []string{"routing", "evaluate", "--explain", "docs"}, want: []string{"claude-code/sonnet", "policy wants balanced"}, not: []string{"code "}},
		{args: []string{"routing", "evaluate", "--explain", "poetry"}, wantErr: "unknown task kind"},
		{args: []string{"routing", "apply"}, want: []string{"applied; 0 pinned"}},
		{args: []string{"routing", "evaluate"}, want: []string{"no changes"}},
		{args: []string{"routing", "show"}, want: []string{"code", "claude-code/opus", "no outcome evidence yet"}},
		{args: []string{"routing", "set", "docs", "codex/gpt-sol", "--effort", "high"}, want: []string{"docs pinned to codex/gpt-sol@high"}},
		{args: []string{"routing", "set", "docs", "codex/gpt-sol", "--effort", "ultra"}, wantErr: "does not support effort"},
		{args: []string{"routing", "set", "docs", "claude-code/nope"}, wantErr: "not routable"},
		{args: []string{"routing", "set", "poetry", "claude-code/opus"}, wantErr: "unknown task kind"},
		{args: []string{"routing", "apply"}, want: []string{"1 pinned kind(s) kept"}},
		{args: []string{"routing", "show"}, want: []string{"pinned: set by hand", "codex/gpt-sol@high"}},
		{args: []string{"routing", "clear", "docs"}, want: []string{"docs cleared"}},
		{args: []string{"routing", "clear", "poetry"}, wantErr: "unknown task kind"},
		{args: []string{"routing", "rollback"}, want: []string{"rolled back"}},
		{args: []string{"routing", "show"}, want: []string{"pinned: set by hand"}},
	}
	for _, s := range steps {
		name := strings.Join(s.args, " ")
		out, err := run(nil, "", s.args...)
		if s.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), s.wantErr) {
				t.Fatalf("%s: err = %v, want %q", name, err, s.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, w := range s.want {
			if !strings.Contains(out, w) {
				t.Fatalf("%s: missing %q in:\n%s", name, w, out)
			}
		}
		for _, n := range s.not {
			if strings.Contains(out, n) {
				t.Fatalf("%s: unexpected %q in:\n%s", name, n, out)
			}
		}
	}
}

func TestRoutingWithNoModels(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	out, err := run(nil, "", "routing", "evaluate")
	if err != nil || !strings.Contains(out, "no enabled models") {
		t.Fatalf("evaluate = %q, %v", out, err)
	}
	if _, err := run(nil, "", "routing", "apply"); err == nil || !strings.Contains(err.Error(), "no enabled models") {
		t.Fatalf("apply err = %v", err)
	}
}

func TestRoutingRejectsInvalidPolicy(t *testing.T) {
	seedModels(t)
	t.Setenv("ORCH_ROUTING_RANKS", "0")
	for _, args := range [][]string{{"routing", "show"}, {"routing", "evaluate"}, {"routing", "apply"},
		{"routing", "rollback"}, {"routing", "set", "code", "claude-code/opus"}, {"routing", "clear", "code"}} {
		if _, err := run(nil, "", args...); err == nil || !strings.Contains(err.Error(), "routing.ranks") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestDispatchRoutingFlags(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	client := &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc", WorkerEnabled: true,
		Route: &orchdv1.Route{TaskKind: "code", Provider: "claude-code", Model: "opus", Confidence: "low"}}}
	out, err := run(factory(client, nil), "", "dispatch", "--kind", "code", "--model", "claude-code/opus", "fix the bug")
	if err != nil {
		t.Fatal(err)
	}
	if client.dispatchReq.GetTaskKind() != "code" || client.dispatchReq.GetModel() != "claude-code/opus" {
		t.Fatalf("request = %+v", client.dispatchReq)
	}
	if !strings.Contains(out, "route: code → claude-code/opus (confidence low") {
		t.Fatalf("output = %q", out)
	}
}

func TestQueuedTasksCannotChangeRouting(t *testing.T) {
	seedModels(t)
	if _, err := run(nil, "", "routing", "apply"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentrun.TaskIDEnv, "task-parent")
	for _, args := range [][]string{
		{"routing", "apply"}, {"routing", "rollback"}, {"routing", "set", "code", "claude-code/haiku"}, {"routing", "clear", "code"},
		{"model", "enable", "codex/gpt-sol"}, {"model", "disable", "claude-code/opus"}, {"model", "tier", "codex/gpt-sol", "fast"},
		{"provider", "enable", "codex"}, {"provider", "disable", "claude-code"},
	} {
		if _, err := run(nil, "", args...); err == nil || !strings.Contains(err.Error(), "cannot change model routing") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	// Reading stays available, and nothing changed.
	out, err := run(nil, "", "routing", "show")
	if err != nil || !strings.Contains(out, "claude-code/opus") || strings.Contains(out, "pinned") {
		t.Fatalf("show = %q, %v", out, err)
	}
	if _, err := run(nil, "", "model", "list"); err != nil {
		t.Fatal(err)
	}
}
