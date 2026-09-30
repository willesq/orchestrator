package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Codex wires the orchestrator into OpenAI Codex CLI: MCP in ~/.codex/config.toml
// (TOML) and hooks in ~/.codex/hooks.json (JSON). Codex's hook engine uses the
// same event names as Claude ("Claude-style engine"), so the event map is reused.
//
// NOTE: the codex hooks.json shape was not field-verified; review the dry-run
// before --apply into Codex.
type Codex struct{}

// Name implements Adapter.
func (Codex) Name() string { return "codex" }

// Edits implements Adapter.
func (c Codex) Edits(bins Bins, home string) []Edit {
	return []Edit{
		codexTOMLEdit(bins, home),
		codexHooksEdit(bins, home),
		aliasEdit(bins.Orch, home),
		plistEdit(bins.Orchd, home),
	}
}

func codexTOMLEdit(bins Bins, home string) Edit {
	path := filepath.Join(home, ".codex", "config.toml")
	return Edit{
		Path:      path,
		Normalize: normalizeTOML,
		Compute: func(old []byte) ([]byte, string, error) {
			root := map[string]any{}
			if len(old) > 0 {
				if err := toml.Unmarshal(old, &root); err != nil {
					return nil, "", fmt.Errorf("parse codex config.toml: %w", err)
				}
			}
			mcp, _ := root["mcp_servers"].(map[string]any)
			if mcp == nil {
				mcp = map[string]any{}
			}
			preserved := 0
			for k := range mcp {
				if k != "orch_rag" && k != "orch_queue" {
					preserved++
				}
			}
			mcp["orch_rag"] = map[string]any{"command": bins.Orch, "args": []any{"mcp", "rag"}}
			mcp["orch_queue"] = map[string]any{"command": bins.Orch, "args": []any{"mcp", "queue"}}
			root["mcp_servers"] = mcp
			out, err := marshalTOML(root)
			if err != nil {
				return nil, "", err
			}
			return out, fmt.Sprintf("mcp (config.toml): registered orch_rag + orch_queue; preserved %d other server(s)", preserved), nil
		},
	}
}

func codexHooksEdit(bins Bins, home string) Edit {
	path := filepath.Join(home, ".codex", "hooks.json")
	return Edit{
		Path:      path,
		Normalize: normalizeJSON,
		Compute: func(old []byte) ([]byte, string, error) {
			root := map[string]any{}
			if len(old) > 0 {
				if err := json.Unmarshal(old, &root); err != nil {
					return nil, "", fmt.Errorf("parse codex hooks.json: %w", err)
				}
			}
			summary := applyHooksWithEvents(root, bins.OrchHook, eventSlugs)
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, summary, nil
		},
	}
}
