package install

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/settings"
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
	{"pre-tool-use", "PreToolUse"},
	{"post-tool-use", "PostToolUse"},
	{"subagent-stop", "SubagentStop"},
	{"cwd-changed", "CwdChanged"},
}

// Units implements Adapter: settings.json (hooks, MCP, permissions,
// attribution), ~/.claude.json (MCP, if present, where user-scope servers like
// serena live), the shell alias, the plist, the orchestrator subagents and
// skills, and the cc-safety-net marketplace, guard rules and plugin.
func (c ClaudeCode) Units(bins Bins, home string) []Unit {
	return append(instructionUnits(home, c.Name()),
		Unit{Name: "claude-code settings", Edits: []Edit{c.settingsEdit(bins, home)}, Leftovers: func() []string {
			return []string{"the credential-file Read deny rules stay in ~/.claude/settings.json (" +
				strings.Join(credentialReadDeny, ", ") + "): they only narrow what agents may read; delete them by hand if you want"}
		}},
		Unit{Name: "claude-code mcp", Edits: []Edit{c.claudeJSONEdit(bins, home)}},
		aliasUnit(bins.Orch, home),
		launchdUnit(bins.Orchd, home),
		claudeAssetsUnit(home),
		safetyNetUnit(home),
	)
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
		// Never removed: the file is Claude Code's, and install never creates it.
		Reverse: func(cur []byte) (Undo, error) {
			return undoJSON(cur, "mcp (~/.claude.json)", false, stripMCP("mcpServers", orchMCPNames()))
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
			// Reason: items the user turned off in orch are left out, so
			// install never puts them back and status never calls them
			// drift. A store that cannot be read stops the plan instead of
			// being taken as "nothing is off", which would re-enable them.
			toggles, err := loadToggles()
			if err != nil {
				return nil, "", fmt.Errorf("%w — refusing to write settings.json, so items turned off in orch stay off; "+
					"fix orch.db or re-run once orchd has released it", err)
			}
			hookSummary := applyClaudeHooks(root, bins.OrchHook, toggles)
			mcpSummary := applyMCP(root, bins.Orch)
			permSummary, err := applyPermissionsWith(root, toggles)
			if err != nil {
				return nil, "", err
			}
			denySummary, err := applyCredentialDenyWith(root, toggles)
			if err != nil {
				return nil, "", err
			}
			attrSummary, err := applyAttribution(root)
			if err != nil {
				return nil, "", err
			}
			out, err := marshalJSON(root)
			if err != nil {
				return nil, "", err
			}
			return out, strings.Join([]string{hookSummary, mcpSummary, permSummary, denySummary, attrSummary}, "\n"), nil
		},
		// Reason: toggles are not read. Uninstall takes out everything of
		// orch's, on or off, and must work without orch.db.
		Reverse: func(cur []byte) (Undo, error) {
			return undoJSON(cur, "settings.json", true, strips(stripHooks(bins.OrchHook),
				stripMCP("mcpServers", orchMCPNames()), stripRules, stripAttribution, stripPluginEnabled))
		},
	}
}

// eventMap maps our orch-hook slug to a harness's hook event name.
type eventMap = []struct{ Slug, Event string }

// hookFieldsFunc returns the harness-specific fields (timeout, async, ...) to
// set on the orch-hook command for an event slug, or nil for none. Each harness
// gets its own, because their hook schemas differ in units and supported keys.
type hookFieldsFunc func(slug string) map[string]any

// hookTimeoutSeconds bounds how long each orch-hook event may run.
//
// Reason: without an explicit timeout Claude Code waits up to 600 s (30 s on
// UserPromptSubmit), so a stalled orchd would freeze every turn, and SessionEnd
// hooks share a 1.5 s budget that can cut session capture short. Claude Code
// raises that budget to match the largest per-hook SessionEnd timeout, up to
// 60 s. The values are ceilings well above normal run times: the transcript
// parsers (session-end, pre-compact) get the most, the prompt and start hooks,
// which make bounded orchd and git calls, get 10 s, and the pure file writers
// and the tool hooks (one orchd call with a 20 ms deadline) 5 s.
var hookTimeoutSeconds = map[string]int{
	"session-start":      10,
	"user-prompt-submit": 10,
	"stop":               5,
	"session-end":        15,
	"pre-compact":        15,
	"pre-tool-use":       5,
	"post-tool-use":      5,
	"subagent-stop":      5,
	"cwd-changed":        5,
}

// claudeHookFields sets Claude Code's per-hook timeout (seconds) and runs
// post-tool-use and cwd-changed in the background.
//
// Reason for async: both only hand events to orchd and print nothing, so
// there is no decision or context for Claude to wait on. Claude Code does not
// enforce timeout on an async hook; it is written anyway so the bound still
// holds if async is ever removed. pre-tool-use stays synchronous: only a hook
// Claude waits for can deny a call, which the tool guard (SP-041) needs.
func claudeHookFields(slug string) map[string]any {
	f := map[string]any{}
	if s, ok := hookTimeoutSeconds[slug]; ok {
		f["timeout"] = s
	}
	if slug == "post-tool-use" || slug == "cwd-changed" {
		f["async"] = true
	}
	return f
}

// applyHooks registers orch-hook using the Claude-style event names.
func applyHooks(root map[string]any, orchHookBin string) string {
	return applyClaudeHooks(root, orchHookBin, settings.Toggles{})
}

// applyClaudeHooks registers orch-hook for Claude Code, leaving alone each
// event whose orch-hook handler the user turned off in orch. The tool hooks
// (pre-tool-use, cwd-changed) are the exception: a settings toggle cannot
// keep them out; only the operator's hooks switch does (D-40).
func applyClaudeHooks(root map[string]any, orchHookBin string, t settings.Toggles) string {
	hooksOff := toolHooksOff()
	skip := func(event, slug string) bool {
		if isToolHookEvent(event, slug) {
			return hooksOff
		}
		return t.Disabled(ProviderClaudeCode, KindHook, HookIdentity(event, slug))
	}
	events, note := applyToolHookSwitch(root, eventSlugs, ProviderClaudeCode)
	return joinSummary(applyHooksWithEvents(root, orchHookBin, events, claudeHookFields, skip), note)
}

// applyHooksWithEvents registers orch-hook for every event, dropping prior
// orchestrator entries and preserving all third-party ones. fields, when not
// nil, adds harness-specific keys to each orch-hook command. skip, when not
// nil, names events to leave exactly as they are. Returns a summary.
func applyHooksWithEvents(root map[string]any, orchHookBin string, events eventMap, fields hookFieldsFunc,
	skip func(event, slug string) bool) string {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	inPlace, added, preserved := 0, 0, 0
	var fieldNotes, skipped []string
	for _, ev := range events {
		if skip != nil && skip(ev.Event, ev.Slug) {
			skipped = append(skipped, ev.Event)
			continue
		}
		cmd := map[string]any{"type": "command", "command": orchHookBin + " " + ev.Slug}
		if fields != nil {
			f := fields(ev.Slug)
			maps.Copy(cmd, f)
			if len(f) > 0 {
				fieldNotes = append(fieldNotes, ev.Event+" "+describeFields(f))
			}
		}
		entry := map[string]any{"hooks": []any{cmd}}
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
	summary := fmt.Sprintf(
		"hooks: wired orch-hook for %d events (%d replaced in place, %d appended); preserved %d third-party entr(ies)",
		len(events)-len(skipped), inPlace, added, preserved)
	if len(fieldNotes) > 0 {
		summary += "\nhooks: orch-hook settings: " + strings.Join(fieldNotes, ", ")
	}
	if len(skipped) > 0 {
		summary += "\nhooks: left " + strings.Join(skipped, ", ") + " alone — turned off in orch"
	}
	return summary
}

// describeFields renders hook fields as "timeout=5 async=true", keys sorted so
// the dry-run output is stable.
func describeFields(f map[string]any) string {
	keys := slices.Sorted(maps.Keys(f))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, f[k])
	}
	return strings.Join(parts, " ")
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
		if !isOrchMCPServer(k) {
			preserved++
		}
	}
	for _, name := range orchMCPServers {
		mcp[name] = map[string]any{"command": orchBin, "args": []any{"mcp", name}}
	}
	root["mcpServers"] = mcp
	return fmt.Sprintf(
		"mcpServers: registered orch %s (-> %s mcp ...); preserved %d other server(s) (e.g. serena)",
		strings.Join(orchMCPServers, " + "), orchBin, preserved)
}

// orchMCPServers names the MCP servers the orchestrator registers with a
// harness. Each is served by `orch mcp <name>`, and each needs a matching
// permission rule, so the list is shared by both steps rather than repeated.
//
// A new name must be normalization-stable: Claude Code normalizes server
// spellings before matching permission rules, while mcpToolRule concatenates, so
// a name containing a hyphen or uppercase would emit a rule that silently
// matches nothing — granting no access while reporting success, and missing a
// deny on the same server. Stick to lowercase and underscores.
var orchMCPServers = []string{"rag", "queue"}

func isOrchMCPServer(name string) bool {
	for _, s := range orchMCPServers {
		if s == name {
			return true
		}
	}
	return false
}

// mcpToolRule is the Claude Code permission rule matching every tool exposed by
// an MCP server (e.g. "mcp__rag__*" covers mcp__rag__rag_search).
//
// Accepted risk: the wildcard also covers rag_delete_source, which is
// irreversible, and its confirm flag is set by the model — so for content the
// store ingested from untrusted text, the interactive prompt was the one guard a
// prompt injection could not forge. Granting per-server was chosen anyway, to
// match the rule shape users already have and to cover tools added later without
// a reinstall. Narrowing means replacing this with a per-server tool allowlist;
// allowsServer already treats an existing wildcard as covering, so doing so
// would not re-prompt anyone. Revisit if the store starts ingesting more
// untrusted input.
//
// Accepted risk (decided 2026-10-30, with the worker on by default): the
// wildcard also pre-approves queue_dispatch, so a prompt injection in an
// interactive session can dispatch a task that the worker then runs
// unattended (permission mode auto, never bypassPermissions) in any directory.
// Kept for frictionless dispatch; the bounds are that queued sessions cannot
// dispatch, queue.max_active caps what is in flight, and a task sitting a day
// without running is stopped rather than started. To close it, deny or ask
// on mcp__queue__queue_dispatch, or turn the worker off.
func mcpToolRule(server string) string { return "mcp__" + server + "__*" }

// exploitFinderSandboxRule pre-authorizes the exploit-finder PoC sandbox wrapper,
// and only that exact command. The wrapper (installed to ~/.claude/agents) hard-codes
// a network-denied, non-root, read-only, cap-dropped, throwaway container and an
// image allowlist, so allowing it to run without a per-run confirmation permits only
// a safely-sandboxed PoC — never a free-form `docker` command. The agent is
// instructed to invoke exactly this shape; anything else (a different docker
// invocation, extra flags, a compound command) falls outside the rule and stays
// gated. The wrapper, not this rule, is the security boundary.
const exploitFinderSandboxRule = "Bash(sh ~/.claude/agents/exploit-finder-sandbox.sh:*)"

// hasRule reports whether the list already contains the exact rule string.
func hasRule(rules []any, rule string) bool {
	for _, r := range rules {
		if s, ok := r.(string); ok && s == rule {
			return true
		}
	}
	return false
}

// allowsServer reports whether the allow list already covers every tool of the
// named server. Claude Code honours the blanket "mcp__*" and the bare server
// form ("mcp__rag") as well as the per-server wildcard, so any of the three
// counts — appending ours would only leave a redundant rule behind.
func allowsServer(allow []any, server string) bool {
	for _, r := range allow {
		s, ok := r.(string)
		if !ok {
			continue
		}
		if s == "mcp__*" || s == "mcp__"+server || s == mcpToolRule(server) {
			return true
		}
	}
	return false
}

// overlapsServer reports whether any rule in the list could match even one tool
// of the named server. It serves both of the categories that outrank allow —
// deny and ask — so neither is treated as the special case.
//
// Reason: the matching here is deliberately broader than allowsServer, and the
// asymmetry is the point. Skipping an allow we would otherwise add needs proof
// that the whole server is already covered, so that check stays exact. Holding
// one back needs only the suspicion of an overlap, because both deny and ask
// outrank allow in Claude Code: a wildcard appended over a single-tool deny, or
// over an ask the user added precisely to keep a destructive tool gated, is dead
// config at best and a bypass if that precedence ever became specificity-based.
func overlapsServer(rules []any, server string) bool {
	prefix := "mcp__" + server
	for _, r := range rules {
		s, ok := r.(string)
		if !ok {
			continue
		}
		if s == "mcp__*" || s == prefix || strings.HasPrefix(s, prefix+"__") {
			return true
		}
	}
	return false
}

// permissionsObject returns the permissions block, or an empty one when absent.
//
// Reason: a value of some other type is reported rather than replaced. The
// discarded-ok form of this assertion conflates "absent" with "present but
// another shape", and the empty fallback would then overwrite whatever the user
// actually had. settings.json is the user's file; refusing is the safe failure.
func permissionsObject(root map[string]any) (map[string]any, error) {
	switch v := root["permissions"].(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("settings.json: permissions is %T, not an object — "+
			"refusing to overwrite it; fix or remove it and re-run", v)
	}
}

// ruleList returns one of the permission rule arrays, with the same refusal as
// permissionsObject for a value of an unexpected shape.
func ruleList(perms map[string]any, key string) ([]any, error) {
	switch v := perms[key].(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	default:
		return nil, fmt.Errorf("settings.json: permissions.%s is %T, not an array — "+
			"refusing to overwrite it; fix or remove it and re-run", key, v)
	}
}

// applyPermissions allows the orchestrator's MCP tools in Claude Code's user
// permission list.
//
// Reason: registering the server is only half the wiring. Under the default
// "ask" permission mode every rag/queue call stops for interactive
// confirmation, so lifecycle hooks and background agents — which have nobody to
// answer the prompt — silently lose access to durable memory. A deny rule beats
// an allow rule in Claude Code, so a server the user has explicitly denied is
// left alone and reported instead of being fought over.
func applyPermissions(root map[string]any) (string, error) {
	return applyPermissionsWith(root, settings.Toggles{})
}

// applyPermissionsWith is applyPermissions leaving out any allow rule the
// user turned off in orch.
func applyPermissionsWith(root map[string]any, t settings.Toggles) (string, error) {
	perms, err := permissionsObject(root)
	if err != nil {
		return "", err
	}
	allow, err := ruleList(perms, "allow")
	if err != nil {
		return "", err
	}
	deny, err := ruleList(perms, "deny")
	if err != nil {
		return "", err
	}
	ask, err := ruleList(perms, "ask")
	if err != nil {
		return "", err
	}

	var added, already, denied, asked, off []string
	for _, server := range orchMCPServers {
		switch {
		case t.Disabled(ProviderClaudeCode, KindPermission, PermissionIdentity("allow", mcpToolRule(server))):
			off = append(off, server)
		case overlapsServer(deny, server):
			denied = append(denied, server)
		case overlapsServer(ask, server):
			asked = append(asked, server)
		case allowsServer(allow, server):
			already = append(already, server)
		default:
			allow = append(allow, mcpToolRule(server))
			added = append(added, server)
		}
	}
	// Pre-authorize the exploit-finder sandbox wrapper so a PoC can run without a
	// per-run confirmation the nested agent has no way to answer. A deny the user
	// added outranks an allow in Claude Code, so an existing deny is left alone.
	//
	// Reason: the rule is only as safe as the wrapper it names, so it follows
	// the exploit-finder toggle: with the agent off its wrapper is removed,
	// and a pre-authorized command naming a missing file would run whatever
	// is put there next.
	bashAdded, bashRemoved := false, false
	finderOn := true
	if on, known := t.State(InstructionsProvider, KindAgent, "exploit-finder"); known {
		finderOn = on
	}
	switch {
	case !finderOn && hasRule(allow, exploitFinderSandboxRule):
		kept := allow[:0:0]
		for _, r := range allow {
			if s, ok := r.(string); !ok || s != exploitFinderSandboxRule {
				kept = append(kept, r)
			}
		}
		allow, bashRemoved = kept, true
	case finderOn && !hasRule(deny, exploitFinderSandboxRule) && !hasRule(ask, exploitFinderSandboxRule) && !hasRule(allow, exploitFinderSandboxRule):
		allow = append(allow, exploitFinderSandboxRule)
		bashAdded = true
	}

	if len(added) > 0 || bashAdded || bashRemoved {
		perms["allow"] = allow
		root["permissions"] = perms
	}

	var notes []string
	if bashAdded {
		notes = append(notes, "added "+exploitFinderSandboxRule+" — exploit-finder PoCs run in its sandboxed container without prompting")
	}
	if bashRemoved {
		notes = append(notes, "removed "+exploitFinderSandboxRule+" — exploit-finder is turned off in orch, so its wrapper is gone")
	}
	if len(added) > 0 {
		rules := make([]string, len(added))
		for i, s := range added {
			rules[i] = mcpToolRule(s)
		}
		notes = append(notes, "added "+strings.Join(rules, " + ")+
			" — these tools will no longer prompt for confirmation, in every project")
	}
	if len(already) > 0 {
		notes = append(notes, strings.Join(already, " + ")+" already allowed")
	}
	// The wording says what was not written rather than claiming the server is
	// unallowed: the user may already have an allow rule of their own, which a
	// deny or ask simply outranks.
	if len(denied) > 0 {
		notes = append(notes,
			"added no allow for "+strings.Join(denied, " + ")+" — an existing deny rule covers it, so those orch MCP tools stay blocked")
	}
	if len(asked) > 0 {
		notes = append(notes,
			"added no allow for "+strings.Join(asked, " + ")+" — an existing ask rule keeps those tools behind a confirmation")
	}
	if len(off) > 0 {
		notes = append(notes, "added no allow for "+strings.Join(off, " + ")+" — turned off in orch")
	}
	return "permissions.allow: " + strings.Join(notes, "; "), nil
}
