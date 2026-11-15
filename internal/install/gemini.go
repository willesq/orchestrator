package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Gemini wires the orchestrator into Gemini CLI (~/.gemini/settings.json — JSON,
// MCP + hooks). Gemini uses different hook event names than Claude/Codex.
//
// NOTE: the hook configuration fields follow Gemini CLI's documented schema
// (docs/hooks/reference.md); the payload schema was not field-verified, so
// review the dry-run before --apply into Gemini.
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

// geminiHookFields sets Gemini CLI's per-hook timeout, which is in
// milliseconds (default 60000), from the shared per-event seconds.
//
// Reason: Gemini's hook configuration accepts only type, command, name,
// timeout and description, so Claude Code's async flag is not written.
// Gemini does not wait for SessionEnd hooks at all (best effort), so the
// timeout there only bounds the process.
func geminiHookFields(slug string) map[string]any {
	s, ok := hookTimeoutSeconds[slug]
	if !ok {
		return nil
	}
	return map[string]any{"timeout": s * 1000}
}

// Units implements Adapter.
func (g Gemini) Units(bins Bins, home string) []Unit {
	return append(instructionUnits(home, g.Name()),
		Unit{Name: "gemini settings", Edits: []Edit{geminiSettingsEdit(bins, home)}},
		aliasUnit(bins.Orch, home),
		launchdUnit(bins.Orchd, home),
	)
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
			hookSummary := applyHooksWithEvents(root, bins.OrchHook, geminiEvents, geminiHookFields, nil)
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, mcpSummary + "\n" + hookSummary, nil
		},
		Reverse: func(cur []byte) (Undo, error) {
			return undoJSON(cur, "gemini settings.json", true, strips(stripMCP("mcpServers", orchMCPNames()), stripHooks(bins.OrchHook)))
		},
	}
}
