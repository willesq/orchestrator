package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// toolHookEvents are the tool-call events spaces need from each harness they
// cover (spaces plan SP-012): every tool call, and for Claude Code every
// change of directory. Spaces cover Claude Code and Codex only (D-20).
var toolHookEvents = map[string]struct {
	config string // the hooks file, relative to home
	events eventMap
}{
	ProviderClaudeCode: {".claude/settings.json", eventMap{{"pre-tool-use", "PreToolUse"}, {"cwd-changed", "CwdChanged"}}},
	"codex":            {".codex/hooks.json", eventMap{{"pre-tool-use", "PreToolUse"}}},
}

// ToolHookCoverage says whether a harness hands its tool calls to orchd.
type ToolHookCoverage struct {
	// Covered is false for a harness spaces do not cover; Wired and Missing
	// are then empty.
	Covered bool
	// Wired and Missing name the harness events whose orch-hook entry is
	// installed or absent.
	Wired, Missing []string
}

// ToolHooks reads the harness's installed hooks file under home and reports
// which of the tool-call events spaces need run orch-hook. A missing file
// wires nothing.
func ToolHooks(harness string, bins Bins, home string) (ToolHookCoverage, error) {
	want, ok := toolHookEvents[harness]
	if !ok {
		return ToolHookCoverage{}, nil
	}
	cov := ToolHookCoverage{Covered: true}
	root := map[string]any{}
	b, err := os.ReadFile(filepath.Join(home, want.config))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cov, err
	default:
		if err := json.Unmarshal(b, &root); err != nil {
			return cov, fmt.Errorf("parse %s: %w", want.config, err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, ev := range want.events {
		if hasCommand(hooks[ev.Event], bins.OrchHook+" "+ev.Slug) {
			cov.Wired = append(cov.Wired, ev.Event)
		} else {
			cov.Missing = append(cov.Missing, ev.Event)
		}
	}
	return cov, nil
}

// hasCommand reports whether a hook event's entries run exactly command.
func hasCommand(entries any, command string) bool {
	list, _ := entries.([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			if hm, _ := h.(map[string]any); hm["command"] == command {
				return true
			}
		}
	}
	return false
}
