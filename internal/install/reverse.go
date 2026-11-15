package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/willesq/orchestrator/internal/safetynet"
)

// The reverse transforms used by each Edit's Reverse. Each takes out exactly
// what the matching Compute puts in and leaves everything else. Where install
// did not record whether an entry was there before (an allow rule, the rtk
// wrapper, attribution.commit), the entry is orch's by definition and goes;
// docs/internals/install.md lists these.

// stripFunc takes orch's part out of a parsed settings tree and returns a
// note per thing it removed (none: nothing of orch's was there).
type stripFunc func(root map[string]any) []string

// undoJSON reverses a structured JSON edit. With removeEmpty, a file left
// holding nothing (an empty object) is removed: install created it.
func undoJSON(cur []byte, what string, removeEmpty bool, strip stripFunc) (Undo, error) {
	if len(cur) == 0 {
		return keepUndo(cur, what+": not present"), nil
	}
	root := map[string]any{}
	if err := json.Unmarshal(cur, &root); err != nil {
		return Undo{}, fmt.Errorf("parse %s: %w — refusing to rewrite it; fix it and re-run", what, err)
	}
	return finishUndo(cur, what, root, removeEmpty, strip(root), marshalJSON)
}

// undoTOML is undoJSON for a TOML file.
func undoTOML(cur []byte, what string, strip stripFunc) (Undo, error) {
	if len(cur) == 0 {
		return keepUndo(cur, what+": not present"), nil
	}
	root := map[string]any{}
	if err := toml.Unmarshal(cur, &root); err != nil {
		return Undo{}, fmt.Errorf("parse %s: %w — refusing to rewrite it; fix it and re-run", what, err)
	}
	return finishUndo(cur, what, root, true, strip(root), marshalTOML)
}

func finishUndo(cur []byte, what string, root map[string]any, removeEmpty bool, notes []string,
	marshal func(map[string]any) ([]byte, error)) (Undo, error) {
	if len(notes) == 0 {
		return keepUndo(cur, what+": nothing of orch's"), nil
	}
	summary := what + ": removed " + strings.Join(notes, "; ")
	if removeEmpty && len(root) == 0 {
		return removeUndo(summary + " — nothing else was in it"), nil
	}
	out, err := marshal(root)
	if err != nil {
		return Undo{}, err
	}
	return Undo{Data: out, Summary: summary}, nil
}

// stripHooks removes every orch-hook entry from every event, as install
// would replace them, dropping events and the hooks object left empty.
func stripHooks(orchHookBin string) stripFunc {
	if orchHookBin == "" {
		orchHookBin = "orch-hook"
	}
	return func(root map[string]any) []string {
		hooks, ok := root["hooks"].(map[string]any)
		if !ok {
			return nil
		}
		removed := 0
		for event, v := range hooks {
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(arr))
			for _, item := range arr {
				if isOrchHookEntry(item, orchHookBin) {
					removed++
					continue
				}
				kept = append(kept, item)
			}
			if len(kept) == len(arr) {
				continue
			}
			if len(kept) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = kept
			}
		}
		if removed == 0 {
			return nil
		}
		if len(hooks) == 0 {
			delete(root, "hooks")
		}
		return []string{fmt.Sprintf("%d orch-hook entr(ies)", removed)}
	}
}

// isOrchHookEntry reports whether every command of a hook entry is one of
// orch's: orch-hook (by its configured path or its name) or a hook script
// of the retired orchestrator checkout.
//
// Reason: stricter than install's isOrchHook, which replaces any entry that
// mentions orch-hook. Uninstall deletes rather than replaces, so a user's
// wrapper whose command merely contains the word must stay.
func isOrchHookEntry(item any, orchHookBin string) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := m["hooks"].([]any)
	if len(inner) == 0 {
		return false
	}
	for _, h := range inner {
		hm, _ := h.(map[string]any)
		cmd, _ := hm["command"].(string)
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return false
		}
		bin := fields[0]
		legacy := len(fields) == 2 && strings.Contains(fields[1], "/.claude/orchestrator/hooks/")
		if bin != orchHookBin && filepath.Base(bin) != "orch-hook" && !legacy {
			return false
		}
	}
	return true
}

// isOrchMCPEntry reports whether an MCP server entry is the one install
// writes: `<dir>/orch mcp <server>`. Reason: a server of the same name that
// runs something else is the user's, and is left.
func isOrchMCPEntry(v any, server string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string)
	args, _ := m["args"].([]any)
	return filepath.Base(cmd) == "orch" && len(args) == 2 && args[0] == "mcp" && args[1] == server
}

// stripMCP removes orch's MCP servers under key (mcpServers, mcp_servers);
// names maps each config name to the `orch mcp` server it runs.
func stripMCP(key string, names map[string]string) stripFunc {
	return func(root map[string]any) []string {
		servers, ok := root[key].(map[string]any)
		if !ok {
			return nil
		}
		var removed []string
		for name, server := range names {
			if isOrchMCPEntry(servers[name], server) {
				delete(servers, name)
				removed = append(removed, name)
			}
		}
		if len(removed) == 0 {
			return nil
		}
		if len(servers) == 0 {
			delete(root, key)
		}
		slices.Sort(removed)
		return []string{key + " " + strings.Join(removed, " + ")}
	}
}

// orchMCPNames maps the Claude Code and Gemini server names to themselves.
func orchMCPNames() map[string]string {
	m := map[string]string{}
	for _, s := range orchMCPServers {
		m[s] = s
	}
	return m
}

// stripRules removes orch's allow rules and drops the list and the
// permissions object left empty. A value of an unexpected shape is left.
//
// Reason: the credential-file Read deny rules are kept. A deny only narrows
// what an agent may read, install did not record whether the user had the
// same rule before, and removing a user's own deny would open their gh,
// netrc and Docker credentials to agents. The uninstall names them instead.
func stripRules(root map[string]any) []string {
	perms, ok := root["permissions"].(map[string]any)
	if !ok {
		return nil
	}
	orch := map[string][]string{"allow": {exploitFinderSandboxRule}}
	for _, s := range orchMCPServers {
		orch["allow"] = append(orch["allow"], mcpToolRule(s))
	}
	var notes []string
	for _, list := range []string{"allow"} {
		arr, ok := perms[list].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(arr))
		var removed []string
		for _, r := range arr {
			if s, ok := r.(string); ok && slices.Contains(orch[list], s) {
				removed = append(removed, s)
				continue
			}
			kept = append(kept, r)
		}
		if len(removed) == 0 {
			continue
		}
		notes = append(notes, "permissions."+list+" "+strings.Join(removed, ", "))
		if len(kept) == 0 {
			delete(perms, list)
		} else {
			perms[list] = kept
		}
	}
	if len(notes) > 0 && len(perms) == 0 {
		delete(root, "permissions")
	}
	return notes
}

// stripAttribution removes the empty attribution.commit install sets.
func stripAttribution(root map[string]any) []string {
	attr, ok := root["attribution"].(map[string]any)
	if !ok {
		return nil
	}
	if s, ok := attr["commit"].(string); !ok || s != "" {
		return nil
	}
	delete(attr, "commit")
	if len(attr) == 0 {
		delete(root, "attribution")
	}
	return []string{`attribution.commit ""`}
}

// stripPluginEnabled removes the enabledPlugins entry Claude Code recorded
// for orch's pinned cc-safety-net, in case the claude CLI left it.
func stripPluginEnabled(root map[string]any) []string {
	e, ok := root["enabledPlugins"].(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := e[safetynet.PluginID]; !ok {
		return nil
	}
	delete(e, safetynet.PluginID)
	if len(e) == 0 {
		delete(root, "enabledPlugins")
	}
	return []string{"enabledPlugins " + safetynet.PluginID}
}

// strips runs several strip functions in order.
func strips(fs ...stripFunc) stripFunc {
	return func(root map[string]any) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f(root)...)
		}
		return out
	}
}

// undoOwnedFile reverses an edit that writes a whole file orch owns:
// the file goes when ours says it is orch's.
func undoOwnedFile(cur []byte, what string, ours func([]byte) bool) (Undo, error) {
	switch {
	case len(cur) == 0:
		return keepUndo(cur, what+": not present"), nil
	case ours != nil && !ours(cur):
		return keepUndo(cur, what+": left in place — it is not orch's"), nil
	}
	return removeUndo(what + ": remove"), nil
}

// cutManagedInstructions takes orch's block out of a harness entry file,
// with the blank line install put before it. Text outside the block is
// kept; broken markers are refused, as install refuses them.
func cutManagedInstructions(cur []byte) (Undo, error) {
	const what = "instructions"
	s := string(cur)
	start, end := strings.Index(s, instructionBegin), strings.Index(s, instructionEnd)
	if start < 0 && end < 0 {
		return keepUndo(cur, what+": no orchestrator-managed block"), nil
	}
	if start < 0 || end < start || strings.Count(s, instructionBegin) != 1 || strings.Count(s, instructionEnd) != 1 {
		return Undo{}, fmt.Errorf("malformed orchestrator instruction markers — refusing to edit; fix them and re-run")
	}
	before, after := s[:start], s[end+len(instructionEnd):]
	after = strings.TrimPrefix(after, "\n")
	// Reason: install adds "\n" before the block: a blank line after text
	// that ends in a newline, or the whole prefix of an empty file.
	if before == "\n" || strings.HasSuffix(before, "\n\n") {
		before = before[:len(before)-1]
	}
	next := before + after
	if next == "" {
		return removeUndo(what + ": remove the managed block — nothing else was in the file"), nil
	}
	return Undo{Data: []byte(next), Summary: what + ": remove the managed block; your own text is kept"}, nil
}

// cutAliasBlock takes orch's alias block out of a shell profile.
func cutAliasBlock(cur []byte) (Undo, error) {
	const what = "alias"
	s := string(cur)
	start := strings.Index(s, aliasBegin)
	if start < 0 {
		return keepUndo(cur, what+": no managed block"), nil
	}
	rel := strings.Index(s[start:], aliasEnd)
	if rel < 0 {
		return Undo{}, fmt.Errorf("orch alias block has no end marker — refusing to edit; fix it and re-run")
	}
	end := start + rel + len(aliasEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	next := s[:start] + s[end:]
	if next == "" {
		return removeUndo(what + ": remove the managed block — nothing else was in the file"), nil
	}
	return Undo{Data: []byte(next), Summary: what + ": remove the managed block"}, nil
}
