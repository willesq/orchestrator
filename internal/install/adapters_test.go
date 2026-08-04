package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestAdapterForAll(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "claude-code", target: "claude-code"},
		{name: "codex", target: "codex"},
		{name: "gemini", target: "gemini"},
		{name: "unknown", target: "vim", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := AdapterFor(tt.target)
			if (err != nil) != tt.wantErr {
				t.Errorf("AdapterFor(%q) err = %v, wantErr %v", tt.target, err, tt.wantErr)
			}
		})
	}
}

func TestAllAdapters(t *testing.T) {
	tests := []struct{ name string }{{name: "returns three adapters"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(AllAdapters()) != 3 {
				t.Errorf("AllAdapters len = %d, want 3", len(AllAdapters()))
			}
		})
	}
}

func TestGeminiApply(t *testing.T) {
	tests := []struct{ name string }{{name: "writes settings.json with MCP + hooks"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(Gemini{}, testBins(), home)
			if err := plan.Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
			if err != nil {
				t.Fatalf("settings.json not written: %v", err)
			}
			var root map[string]any
			if err := json.Unmarshal(b, &root); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			mcp, _ := root["mcpServers"].(map[string]any)
			if _, ok := mcp["rag"]; !ok {
				t.Errorf("rag MCP not registered")
			}
			hooks, _ := root["hooks"].(map[string]any)
			if _, ok := hooks["BeforeAgent"]; !ok {
				t.Errorf("BeforeAgent hook not registered")
			}
		})
	}
}

func TestCodexApply(t *testing.T) {
	tests := []struct{ name string }{{name: "writes config.toml MCP + hooks.json"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(Codex{}, testBins(), home)
			if err := plan.Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
			if err != nil {
				t.Fatalf("config.toml not written: %v", err)
			}
			var root map[string]any
			if err := toml.Unmarshal(b, &root); err != nil {
				t.Fatalf("invalid TOML: %v", err)
			}
			mcp, _ := root["mcp_servers"].(map[string]any)
			if _, ok := mcp["orch_rag"]; !ok {
				t.Errorf("orch_rag not registered in config.toml")
			}
			hb, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
			if err != nil {
				t.Fatalf("hooks.json not written: %v", err)
			}
			if !strings.Contains(string(hb), "orch-hook") {
				t.Errorf("hooks.json missing orch-hook")
			}
		})
	}
}

func TestCodexPreservesExistingMCP(t *testing.T) {
	tests := []struct{ name string }{{name: "preserves a third-party TOML mcp server"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, ".codex", "config.toml")
			_ = os.MkdirAll(filepath.Dir(cfg), 0o755)
			_ = os.WriteFile(cfg, []byte("[mcp_servers.serena]\ncommand = \"serena\"\n"), 0o644)

			plan := BuildPlan(Codex{}, testBins(), home)
			if err := plan.Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			b, _ := os.ReadFile(cfg)
			var root map[string]any
			_ = toml.Unmarshal(b, &root)
			mcp := root["mcp_servers"].(map[string]any)
			if _, ok := mcp["serena"]; !ok {
				t.Errorf("serena not preserved in config.toml")
			}
			if _, ok := mcp["orch_rag"]; !ok {
				t.Errorf("orch_rag not added")
			}
		})
	}
}
