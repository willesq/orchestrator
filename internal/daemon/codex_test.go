package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/workflows"
)

func TestCodexOptions(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	off := codexOptions(config.QueueCodexSettings{Bin: "/opt/codex"}, "")
	if off.Bin != "" || off.Sandbox != "workspace-write" || off.StateDir == "" {
		t.Fatalf("disabled = %+v", off)
	}
	on := codexOptions(config.QueueCodexSettings{Enabled: true, Bin: "/opt/codex", Sandbox: "read-only", Network: true}, "")
	if on.Bin != "/opt/codex" || on.Sandbox != "read-only" || !on.Network {
		t.Fatalf("enabled = %+v", on)
	}
}

func TestCodexRunnable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		w    config.QueueWorkerSettings
		want bool
	}{
		{name: "worker off", w: config.QueueWorkerSettings{Codex: config.QueueCodexSettings{Enabled: true, Bin: bin}}},
		{name: "codex off", w: config.QueueWorkerSettings{Enabled: true, Codex: config.QueueCodexSettings{Bin: bin}}},
		{name: "on", w: config.QueueWorkerSettings{Enabled: true, Codex: config.QueueCodexSettings{Enabled: true, Bin: bin}}, want: true},
	}
	for _, tt := range tests {
		if got := codexRunnable(tt.w); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
	if workerRunnable(false)("codex") || !workerRunnable(true)("codex") || !workerRunnable(false)("claude-code") || workerRunnable(true)("gemini") {
		t.Error("workerRunnable gates providers wrongly")
	}
}

func TestTaskRunnerCodexRechecks(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reg := models.NewRegistry(conn)
	ctx := context.Background()
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "codex", State: models.StateEnabled}, models.StateEnabled,
		[]models.Model{{Slug: "gpt-sol", Source: "cache", SupportedEfforts: []string{"low", "high"}}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetModelState(ctx, "codex", "gpt-sol", models.StateEnabled); err != nil {
		t.Fatal(err)
	}
	in := func(effort string) workflows.AgentTaskInput {
		return workflows.AgentTaskInput{ID: "t", Agent: "dev",
			Route: workflows.Route{Kind: "code", Provider: "codex", Model: "gpt-sol", Effort: effort}}
	}
	withCodex := taskRunner{r: &agentrun.Runner{Codex: agentrun.CodexOptions{Bin: "/opt/codex"}}, models: reg}
	if got := withCodex.task(ctx, in("high")); got.Provider != "codex" || got.Model != "gpt-sol" || got.Effort != "high" {
		t.Fatalf("routable = %+v", got)
	}
	if got := withCodex.task(ctx, in("ultra")); got.Effort != "" || got.Model != "gpt-sol" {
		t.Fatalf("unsupported effort kept: %+v", got)
	}
	noCodex := taskRunner{r: &agentrun.Runner{}, models: reg}
	if got := noCodex.task(ctx, in("high")); got.Provider != "" || got.Model != "" || got.Effort != "" {
		t.Fatalf("codex off, task still routed to it: %+v", got)
	}
	if err := reg.SetModelState(ctx, "codex", "gpt-sol", models.StateDisabled); err != nil {
		t.Fatal(err)
	}
	if got := withCodex.task(ctx, in("high")); got.Model != "" || got.Effort != "" {
		t.Fatalf("disabled model kept: %+v", got)
	}
}
