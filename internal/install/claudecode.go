package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ClaudeCode wires the orchestrator into Claude Code (~/.claude).
type ClaudeCode struct{}

// Name implements Adapter.
func (ClaudeCode) Name() string { return "claude-code" }

// eventSlugs maps our orch-hook event slug to the Claude Code hook event name.
var eventSlugs = []struct{ Slug, Event string }{
	{"session-start", "SessionStart"},
	{"user-prompt-submit", "UserPromptSubmit"},
	{"stop", "Stop"},
	{"session-end", "SessionEnd"},
	{"pre-compact", "PreCompact"},
	{"post-tool-use", "PostToolUse"},
	{"subagent-stop", "SubagentStop"},
}

// Edits implements Adapter: settings.json (hooks + MCP), ~/.claude.json (MCP, if
// present, where user-scope servers like serena live), the shell alias, the plist.
func (c ClaudeCode) Edits(bins Bins, home string) []Edit {
	return []Edit{
		c.settingsEdit(bins, home),
		c.claudeJSONEdit(bins, home),
		aliasEdit(bins.Orch, home),
		plistEdit(bins.Orchd, home),
	}
}

// claudeJSONEdit updates rag/queue MCP registration in ~/.claude.json ONLY if the
// file already exists — so Claude picks up the Go servers whichever file it reads,
// while preserving serena and everything else. Never creates the file.
func (ClaudeCode) claudeJSONEdit(bins Bins, home string) Edit {
	path := filepath.Join(home, ".claude.json")
	return Edit{
		Path:      path,
		Normalize: normalizeJSON,
		Compute: func(old []byte) ([]byte, string, error) {
			if len(old) == 0 {
				return old, "mcp (~/.claude.json): skipped — file not present", nil
			}
			root := map[string]any{}
			if err := json.Unmarshal(old, &root); err != nil {
				return nil, "", fmt.Errorf("parse .claude.json: %w", err)
			}
			summary := applyMCP(root, bins.Orch)
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, "mcp (~/.claude.json): " + summary, nil
		},
	}
}

func (ClaudeCode) settingsEdit(bins Bins, home string) Edit {
	path := filepath.Join(home, ".claude", "settings.json")
	return Edit{
		Path:      path,
		Normalize: normalizeJSON,
		Compute: func(old []byte) ([]byte, string, error) {
			root := map[string]any{}
			if len(old) > 0 {
				if err := json.Unmarshal(old, &root); err != nil {
					return nil, "", fmt.Errorf("parse settings.json: %w", err)
				}
			}
			hookSummary := applyHooks(root, bins.OrchHook)
			mcpSummary := applyMCP(root, bins.Orch)
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, hookSummary + "\n" + mcpSummary, nil
		},
	}
}

// eventMap maps our orch-hook slug to a harness's hook event name.
type eventMap = []struct{ Slug, Event string }

// applyHooks registers orch-hook using the Claude-style event names.
func applyHooks(root map[string]any, orchHookBin string) string {
	return applyHooksWithEvents(root, orchHookBin, eventSlugs)
}

// applyHooksWithEvents registers orch-hook for every event, dropping prior
// orchestrator entries and preserving all third-party ones. Returns a summary.
func applyHooksWithEvents(root map[string]any, orchHookBin string, events eventMap) string {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	inPlace, added, preserved := 0, 0, 0
	for _, ev := range events {
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": orchHookBin + " " + ev.Slug}},
		}
		arr, _ := hooks[ev.Event].([]any)
		var kept []any
		placed := false
		for _, item := range arr {
			if !isOrchHook(item, orchHookBin) {
				kept = append(kept, item)
				preserved++
				continue
			}
			// Reason: reuse the first orchestrator slot instead of appending. A
			// harness or third-party tool that registers its own hook after ours
			// would otherwise make the wiring look drifted forever, and applying
			// would silently reshuffle hook execution order.
			if !placed {
				kept = append(kept, entry)
				placed = true
				inPlace++
			}
		}
		if !placed {
			kept = append(kept, entry)
			added++
		}
		hooks[ev.Event] = kept
	}
	root["hooks"] = hooks
	return fmt.Sprintf(
		"hooks: wired orch-hook for %d events (%d replaced in place, %d appended); preserved %d third-party entr(ies)",
		len(events), inPlace, added, preserved)
}

// isOrchHook reports whether a hook entry belongs to the orchestrator (old Python
// scripts or a prior orch-hook install), so it can be replaced idempotently.
func isOrchHook(item any, orchHookBin string) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := m["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, orchHookBin) ||
			strings.Contains(cmd, "orch-hook") ||
			strings.Contains(cmd, "orchestrator/hooks/") {
			return true
		}
	}
	return false
}

// applyMCP registers the orchestrator's rag + queue MCP servers, preserving others.
func applyMCP(root map[string]any, orchBin string) string {
	mcp, _ := root["mcpServers"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	preserved := 0
	for k := range mcp {
		if k != "rag" && k != "queue" {
			preserved++
		}
	}
	mcp["rag"] = map[string]any{"command": orchBin, "args": []any{"mcp", "rag"}}
	mcp["queue"] = map[string]any{"command": orchBin, "args": []any{"mcp", "queue"}}
	root["mcpServers"] = mcp
	return fmt.Sprintf(
		"mcpServers: registered orch rag + queue (-> %s mcp ...); preserved %d other server(s) (e.g. serena)",
		orchBin, preserved)
}
