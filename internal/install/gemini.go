package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Gemini wires the orchestrator into Gemini CLI (~/.gemini/settings.json — JSON,
// MCP + hooks). Gemini uses different hook event names than Claude/Codex.
//
// NOTE: the exact Gemini hook payload schema was not field-verified; review the
// dry-run before --apply into Gemini.
type Gemini struct{}

// Name implements Adapter.
func (Gemini) Name() string { return "gemini" }

// geminiEvents maps orch-hook slugs to Gemini CLI hook event names (subset).
var geminiEvents = eventMap{
	{"session-start", "SessionStart"},
	{"user-prompt-submit", "BeforeAgent"},
	{"session-end", "SessionEnd"},
	{"post-tool-use", "AfterTool"},
}

// Edits implements Adapter.
func (g Gemini) Edits(bins Bins, home string) []Edit {
	edits := append(instructionEdits(home, g.Name()), []Edit{
		geminiSettingsEdit(bins, home),
		aliasEdit(bins.Orch, home),
		plistEdit(bins.Orchd, home),
	}...)
	edits = append(edits, toolEdits(home)...)
	return edits
}

func geminiSettingsEdit(bins Bins, home string) Edit {
	path := filepath.Join(home, ".gemini", "settings.json")
	return Edit{
		Path:      path,
		Normalize: normalizeJSON,
		Compute: func(old []byte) ([]byte, string, error) {
			root := map[string]any{}
			if len(old) > 0 {
				if err := json.Unmarshal(old, &root); err != nil {
					return nil, "", fmt.Errorf("parse gemini settings.json: %w", err)
				}
			}
			mcpSummary := applyMCP(root, bins.Orch)
			hookSummary := applyHooksWithEvents(root, bins.OrchHook, geminiEvents)
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, mcpSummary + "\n" + hookSummary, nil
		},
	}
}
