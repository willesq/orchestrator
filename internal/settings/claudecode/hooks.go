package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/settings"
)

// Events are the hook events Claude Code documents
// (https://code.claude.com/docs/en/hooks), the only ones a hook can be added
// for. Hooks already in the file are listed whatever their event.
var Events = []string{
	"SessionStart", "Setup", "UserPromptSubmit", "UserPromptExpansion", "PreToolUse", "PermissionRequest",
	"PermissionDenied", "PostToolUse", "PostToolUseFailure", "PostToolBatch", "Notification", "MessageDisplay",
	"SubagentStart", "SubagentStop", "TaskCreated", "TaskCompleted", "Stop", "StopFailure", "TeammateIdle",
	"InstructionsLoaded", "ConfigChange", "CwdChanged", "DirectoryAdded", "FileChanged", "WorktreeCreate",
	"WorktreeRemove", "PreCompact", "PostCompact", "PreModelSwitch", "PostModelSwitch", "Elicitation",
	"ElicitationResult", "SessionEnd",
}

// noMatcherEvents ignore a matcher, so adding one there is a mistake.
var noMatcherEvents = []string{
	"UserPromptSubmit", "PostToolBatch", "Stop", "TeammateIdle", "TaskCreated", "TaskCompleted",
	"CwdChanged", "WorktreeCreate", "WorktreeRemove", "MessageDisplay",
}

// ifEvents are the tool events that honor a handler's "if"; on any other
// event a hook with "if" never runs.
var ifEvents = []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "PermissionDenied"}

// Limits on added hooks. Generous for real use; they keep one request from
// writing an unbounded command into the user's settings.
const (
	maxCommand    = 8 << 10
	maxMatcher    = 512
	maxTimeoutSec = 24 * 60 * 60
)

// hookPayload is a stored hook: its event, its group's matcher when the
// group has one, and the handler object exactly as it was in the file.
type hookPayload struct {
	Event   string         `json:"event"`
	Matcher *string        `json:"matcher,omitempty"`
	Handler map[string]any `json:"handler"`
}

// hookRef is one handler found in the file, and where.
type hookRef struct {
	item         settings.Item
	event        string
	group, index int
}

// hookRefs lists every hook handler in the file in a stable order: events
// by name, then groups and handlers as written. Identical handlers (same
// event, matcher and fields) share one identity: they are one item, as a
// rule written twice in a permission list is.
func hookRefs(root map[string]any) []hookRef {
	hooks, _ := root["hooks"].(map[string]any)
	var refs []hookRef
	for _, event := range sortedKeys(hooks) {
		groups, _ := hooks[event].([]any)
		for gi, g := range groups {
			group, _ := g.(map[string]any)
			if group == nil {
				continue
			}
			var matcher *string
			if m, ok := group["matcher"].(string); ok {
				matcher = &m
			}
			handlers, _ := group["hooks"].([]any)
			for hi, h := range handlers {
				handler, _ := h.(map[string]any)
				if handler == nil {
					continue
				}
				p := hookPayload{Event: event, Matcher: matcher, Handler: handler}
				command, _ := handler["command"].(string)
				origin := settings.OriginFound
				if _, ok := install.OrchHookSlugFor(event, command); ok {
					origin = settings.OriginOrch
				}
				refs = append(refs, hookRef{
					item: settings.Item{
						Kind: install.KindHook, Identity: hookIdentity(p), Payload: mustJSON(p),
						Enabled: true, Origin: origin,
					},
					event: event, group: gi, index: hi,
				})
			}
		}
	}
	return refs
}

// hookIdentity names a handler. orch-hook's handlers are named by event and
// subcommand, as install names them; any other by a hash of its matcher and
// handler, so editing a hook in the file makes it a new item.
func hookIdentity(p hookPayload) string {
	command, _ := p.Handler["command"].(string)
	if slug, ok := install.OrchHookSlugFor(p.Event, command); ok && p.Matcher == nil {
		return install.HookIdentity(p.Event, slug)
	}
	sum := sha256.Sum256(mustJSON(struct {
		Matcher *string        `json:"matcher"`
		Handler map[string]any `json:"handler"`
	}{p.Matcher, p.Handler}))
	return p.Event + "/" + hex.EncodeToString(sum[:6])
}

func hookItems(root map[string]any) []settings.Item {
	var out []settings.Item
	seen := map[string]bool{}
	for _, r := range hookRefs(root) {
		if !seen[r.item.Identity] {
			seen[r.item.Identity] = true
			out = append(out, r.item)
		}
	}
	return out
}

// applyHook removes a handler from the file, or adds its stored payload back:
// into the first group with the same matcher, else as a new group.
func applyHook(root map[string]any, it settings.Item, enabled bool) (bool, error) {
	hooks, err := object(root, "hooks")
	if err != nil {
		return false, err
	}
	var found []hookRef
	for _, r := range hookRefs(root) {
		if r.item.Identity == it.Identity {
			found = append(found, r)
		}
	}
	if !enabled {
		// Every copy goes, last first, so earlier positions stay valid.
		for i := len(found) - 1; i >= 0; i-- {
			removeHandler(hooks, found[i].event, found[i].group, found[i].index)
		}
		return len(found) > 0, nil
	}
	if len(found) > 0 {
		return false, nil
	}
	// Numbers are kept as written, so the handler goes back byte for byte
	// and keeps its identity.
	var p hookPayload
	dec := json.NewDecoder(bytes.NewReader(it.Payload))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil || p.Event == "" || p.Handler == nil {
		return false, fmt.Errorf("%w: stored hook %q has no usable payload", settings.ErrInvalid, it.Identity)
	}
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	groups, ok := hooks[p.Event].([]any)
	if !ok && hooks[p.Event] != nil {
		return false, fmt.Errorf("%w: settings.json: hooks.%s is not an array — refusing to rewrite it", settings.ErrInvalid, p.Event)
	}
	for _, g := range groups {
		group, _ := g.(map[string]any)
		if group == nil || !sameMatcher(group, p.Matcher) {
			continue
		}
		if handlers, ok := group["hooks"].([]any); ok {
			group["hooks"] = append(handlers, p.Handler)
			return true, nil
		}
	}
	group := map[string]any{"hooks": []any{p.Handler}}
	if p.Matcher != nil {
		group["matcher"] = *p.Matcher
	}
	hooks[p.Event] = append(groups, group)
	return true, nil
}

func sameMatcher(group map[string]any, matcher *string) bool {
	m, has := group["matcher"].(string)
	if matcher == nil {
		_, present := group["matcher"]
		return !present
	}
	return has && m == *matcher
}

// removeHandler deletes one handler, then its group if nothing else is left
// in it, then the event if it has no groups left.
func removeHandler(hooks map[string]any, event string, gi, hi int) {
	groups := hooks[event].([]any)
	group := groups[gi].(map[string]any)
	handlers := group["hooks"].([]any)
	handlers = slices.Delete(slices.Clone(handlers), hi, hi+1)
	group["hooks"] = handlers
	if len(handlers) > 0 {
		return
	}
	for k := range group {
		if k != "hooks" && k != "matcher" {
			// Reason: a key orch does not know is the user's; keep the group.
			return
		}
	}
	groups = slices.Delete(slices.Clone(groups), gi, gi+1)
	if len(groups) == 0 {
		delete(hooks, event)
		return
	}
	hooks[event] = groups
}

// validateHook builds a command hook from an add form.
func validateHook(f map[string]string) (settings.Item, error) {
	event := strings.TrimSpace(f["event"])
	if !slices.Contains(Events, event) {
		return settings.Item{}, settings.Invalidf("event %q is not a Claude Code hook event", event)
	}
	command := strings.TrimSpace(f["command"])
	switch {
	case command == "":
		return settings.Item{}, settings.Invalidf("command is required")
	case len(command) > maxCommand:
		return settings.Item{}, settings.Invalidf("command is %d bytes; the limit is %d", len(command), maxCommand)
	case !plainText(command, true):
		return settings.Item{}, settings.Invalidf("command holds control characters")
	}
	handler := map[string]any{"type": "command", "command": command}
	var matcher *string
	if m := strings.TrimSpace(f["matcher"]); m != "" {
		switch {
		case slices.Contains(noMatcherEvents, event):
			return settings.Item{}, settings.Invalidf("%s ignores a matcher; leave it empty", event)
		case len(m) > maxMatcher || !plainText(m, false):
			return settings.Item{}, settings.Invalidf("matcher must be one line of at most %d characters", maxMatcher)
		}
		matcher = &m
	}
	if cond := strings.TrimSpace(f["if"]); cond != "" {
		if !slices.Contains(ifEvents, event) {
			return settings.Item{}, settings.Invalidf("\"if\" applies only to %s; on %s the hook would never run",
				strings.Join(ifEvents, ", "), event)
		}
		if err := checkRule(cond); err != nil {
			return settings.Item{}, settings.Invalidf("if: %v", err)
		}
		handler["if"] = cond
	}
	if t := strings.TrimSpace(f["timeout"]); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > maxTimeoutSec {
			return settings.Item{}, settings.Invalidf("timeout must be a whole number of seconds from 1 to %d", maxTimeoutSec)
		}
		handler["timeout"] = json.Number(strconv.Itoa(n))
	}
	switch a := strings.TrimSpace(f["async"]); a {
	case "", "false":
	case "true":
		handler["async"] = true
	default:
		return settings.Item{}, settings.Invalidf("async must be true or false")
	}
	p := hookPayload{Event: event, Matcher: matcher, Handler: handler}
	return settings.Item{Kind: install.KindHook, Identity: hookIdentity(p), Payload: mustJSON(p),
		Enabled: true, Origin: settings.OriginYou}, nil
}

// describeHook is a one-line summary: what the handler runs, its matcher and
// its condition.
func describeHook(h hookPayload) string {
	what := ""
	for _, k := range []string{"command", "prompt", "url"} {
		if s, ok := h.Handler[k].(string); ok && s != "" {
			what = s
			break
		}
	}
	if what == "" {
		server, _ := h.Handler["server"].(string)
		tool, _ := h.Handler["tool"].(string)
		what = strings.TrimSpace(server + " " + tool)
	}
	if h.Matcher != nil && *h.Matcher != "" && *h.Matcher != "*" {
		what = "[" + *h.Matcher + "] " + what
	}
	if cond, ok := h.Handler["if"].(string); ok && cond != "" {
		what += " (if " + cond + ")"
	}
	return what
}

// plainText reports valid UTF-8 free of control and format characters;
// multiline allows newlines and tabs.
func plainText(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		// Reason: format characters (bidi overrides, zero-width marks) make
		// a command display as something other than what runs.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
