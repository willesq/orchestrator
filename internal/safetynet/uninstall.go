package safetynet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// UnmergeRuleConfig is MergeRuleConfig's reverse: it takes orch-guards out
// of rules and the rtk wrapper out of transparent_wrappers, dropping either
// list once empty, and keeps every other key, source, override and wrapper.
// A file left holding only version 1 is removed (remove is true): install
// created it. An unparseable file is an error, as for the merge.
func UnmergeRuleConfig(cur []byte) (next []byte, remove bool, summary string, err error) {
	const what = "rule.json"
	if len(bytes.TrimSpace(cur)) == 0 {
		return cur, false, what + ": not present", nil
	}
	root := map[string]any{}
	if err := json.Unmarshal(cur, &root); err != nil {
		return nil, false, "", fmt.Errorf("parse rule.json: %w — refusing to rewrite it; fix it and re-run", err)
	}
	var notes []string
	for _, f := range []struct{ key, drop, what string }{
		{"rules", RulebookName, "rulebook source"},
		{"transparent_wrappers", RTKWrapper, "transparent wrapper"},
	} {
		list, err := stringList(root, f.key)
		if err != nil {
			return nil, false, "", err
		}
		if !slices.Contains(list, f.drop) {
			continue
		}
		var kept []any
		for _, s := range list {
			if s != f.drop {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(root, f.key)
		} else {
			root[f.key] = kept
		}
		notes = append(notes, fmt.Sprintf("removed %s %q", f.what, f.drop))
	}
	if len(notes) == 0 {
		return cur, false, what + ": lists nothing of orch's", nil
	}
	summary = what + ": " + strings.Join(notes, "; ")
	if v, ok := root["version"].(float64); len(root) == 1 && ok && v == 1 {
		return nil, true, summary + " — nothing else was in it", nil
	}
	out, err := marshalJSON(root)
	if err != nil {
		return nil, false, "", err
	}
	return out, false, summary + "; kept every other source, override and wrapper", nil
}

// UninstallCommands returns the claude commands that take orch's plugin and
// marketplace out of Claude Code: uninstall the user-scope plugin, then
// unregister the marketplace. Competing copies from other marketplaces are
// the user's and are left. An already-clean state plans nothing.
func UninstallCommands(st State) []Command {
	var cmds []Command
	if st.Installed != nil {
		cmds = append(cmds, Command{[]string{"plugin", "uninstall", PluginID, "--scope", "user"},
			"uninstall " + PluginID})
	}
	if st.MarketplaceKnown {
		cmds = append(cmds, Command{[]string{"plugin", "marketplace", "remove", MarketplaceName},
			"unregister the orch marketplace " + MarketplaceName})
	}
	return cmds
}
