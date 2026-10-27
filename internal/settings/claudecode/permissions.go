package claudecode

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
)

// permissionLists are the permission rule arrays, in Claude Code's order of
// precedence reversed for display: allow, then ask, then deny.
var permissionLists = []string{"allow", "ask", "deny"}

// maxRule bounds an added rule's length.
const maxRule = 1024

// ruleRe is a permission rule's shape: a tool name (letters, digits, _, -,
// and the * of an MCP wildcard such as mcp__rag__*), optionally followed by
// a non-empty specifier in parentheses that runs to the end of the rule.
var ruleRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_*-]*(\(.+\))?$`)

type permissionPayload struct {
	List string `json:"list"`
	Rule string `json:"rule"`
}

type pluginPayload struct {
	Plugin string `json:"plugin"`
}

// permissionItems lists each distinct rule of each list. A rule written
// twice in one list is one item, and turning it off removes both.
func permissionItems(root map[string]any) []settings.Item {
	perms, _ := root["permissions"].(map[string]any)
	orch := install.OrchPermissionRules()
	var out []settings.Item
	for _, list := range permissionLists {
		rules, _ := perms[list].([]any)
		seen := map[string]bool{}
		for _, r := range rules {
			rule, ok := r.(string)
			if !ok || seen[rule] {
				continue
			}
			seen[rule] = true
			origin := settings.OriginFound
			if slices.Contains(orch, install.OrchRule{List: list, Rule: rule}) {
				origin = settings.OriginOrch
			}
			out = append(out, settings.Item{
				Kind: install.KindPermission, Identity: install.PermissionIdentity(list, rule),
				Payload: mustJSON(permissionPayload{List: list, Rule: rule}), Enabled: true, Origin: origin,
			})
		}
	}
	return out
}

// applyPermission removes every copy of a rule from its list, or appends it.
func applyPermission(root map[string]any, it settings.Item, enabled bool) (bool, error) {
	var p permissionPayload
	if err := jsonInto(it.Payload, &p); err != nil || !slices.Contains(permissionLists, p.List) || p.Rule == "" {
		return false, fmt.Errorf("%w: stored permission %q has no usable payload", settings.ErrInvalid, it.Identity)
	}
	perms, err := object(root, "permissions")
	if err != nil {
		return false, err
	}
	var rules []any
	switch v := perms[p.List].(type) {
	case nil:
	case []any:
		rules = v
	default:
		return false, fmt.Errorf("%w: settings.json: permissions.%s is %T, not an array — refusing to rewrite it",
			settings.ErrInvalid, p.List, v)
	}
	has := slices.ContainsFunc(rules, func(r any) bool { s, _ := r.(string); return s == p.Rule })
	switch {
	case enabled && has, !enabled && !has:
		return false, nil
	case enabled:
		rules = append(rules, p.Rule)
	default:
		rules = slices.DeleteFunc(slices.Clone(rules), func(r any) bool { s, _ := r.(string); return s == p.Rule })
	}
	if perms == nil {
		perms = map[string]any{}
		root["permissions"] = perms
	}
	perms[p.List] = rules
	return true, nil
}

// validatePermission builds a rule from an add form.
func validatePermission(f map[string]string) (settings.Item, error) {
	list := strings.TrimSpace(f["list"])
	if !slices.Contains(permissionLists, list) {
		return settings.Item{}, settings.Invalidf("list must be one of %s", strings.Join(permissionLists, ", "))
	}
	rule := strings.TrimSpace(f["rule"])
	if err := checkRule(rule); err != nil {
		return settings.Item{}, settings.Invalidf("rule: %v", err)
	}
	return settings.Item{
		Kind: install.KindPermission, Identity: install.PermissionIdentity(list, rule),
		Payload: mustJSON(permissionPayload{List: list, Rule: rule}), Enabled: true, Origin: settings.OriginYou,
	}, nil
}

// checkRule checks a permission rule's shape.
func checkRule(rule string) error {
	switch {
	case rule == "":
		return fmt.Errorf("a rule is required")
	case len(rule) > maxRule:
		return fmt.Errorf("%d bytes; the limit is %d", len(rule), maxRule)
	case !plainText(rule, false):
		return fmt.Errorf("holds control characters")
	case !ruleRe.MatchString(rule):
		return fmt.Errorf("%q is not a tool name with an optional (specifier), such as Bash(git status) or mcp__rag__*", rule)
	}
	return nil
}

// pluginItems lists enabledPlugins. Claude Code documents only boolean
// values; anything else is left out rather than guessed at.
func pluginItems(root map[string]any) []settings.Item {
	plugins, _ := root["enabledPlugins"].(map[string]any)
	var out []settings.Item
	for _, id := range sortedKeys(plugins) {
		on, ok := plugins[id].(bool)
		if !ok {
			continue
		}
		origin := settings.OriginFound
		if id == safetynet.PluginID {
			origin = settings.OriginOrch
		}
		out = append(out, settings.Item{Kind: install.KindPlugin, Identity: id,
			Payload: mustJSON(pluginPayload{Plugin: id}), Enabled: on, Origin: origin})
	}
	return out
}

// applyPlugin sets a plugin's enabledPlugins entry, as claude plugin
// enable and disable do. Only entries already in the file are toggled.
func applyPlugin(root map[string]any, it settings.Item, enabled bool) (bool, error) {
	plugins, err := object(root, "enabledPlugins")
	if err != nil {
		return false, err
	}
	cur, ok := plugins[it.Identity].(bool)
	if !ok {
		return false, fmt.Errorf("%w: plugin %q is not in enabledPlugins", settings.ErrNotFound, it.Identity)
	}
	if cur == enabled {
		return false, nil
	}
	plugins[it.Identity] = enabled
	return true, nil
}
