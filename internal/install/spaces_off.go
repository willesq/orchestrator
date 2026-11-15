package install

import (
	"fmt"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/config"
)

// Memory spaces are opt-in (spaces.enabled, off by default). Off, install
// writes the hooks orch wrote before spaces and nothing more: no tool hooks
// (pre-tool-use, cwd-changed), and orch's own entries for them, left by an
// install made while spaces were on, are removed. Third-party entries for
// those events are kept.

// loadSpacesEnabled reads the spaces switch; tests replace it.
var loadSpacesEnabled = config.SpacesEnabled

// SetSpacesLoader replaces how install reads the spaces switch and returns
// a function restoring the previous loader, for tests in other packages,
// which must never read the developer's own orch.db.
func SetSpacesLoader(f func() (bool, error)) (restore func()) {
	prev := loadSpacesEnabled
	loadSpacesEnabled = f
	return func() { loadSpacesEnabled = prev }
}

// SpacesOff reports whether memory spaces are switched off. Reason, fail
// closed: a switch that cannot be read counts as on, so install keeps the
// tool hooks a policy may depend on.
func SpacesOff() bool {
	on, err := loadSpacesEnabled()
	return err == nil && !on
}

// withoutToolHooks is events less the tool hooks spaces add for harness.
func withoutToolHooks(events eventMap, harness string) eventMap {
	tool := toolHookEvents[harness].events
	return slices.DeleteFunc(slices.Clone(events), func(e struct{ Slug, Event string }) bool {
		return slices.Contains(tool, e)
	})
}

// dropToolHooks removes orch's own tool hook entries for harness from a
// hooks file's root and says what it removed. Only a command exactly as
// install writes it ("<dir>/orch-hook <slug>") is orch's: anything else in
// those events is a third party's and stays.
func dropToolHooks(root map[string]any, harness string) string {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return ""
	}
	var removed []string
	for _, ev := range toolHookEvents[harness].events {
		list, ok := hooks[ev.Event].([]any)
		if !ok {
			continue
		}
		var kept []any
		n := 0
		for _, item := range list {
			m, ok := item.(map[string]any)
			inner, _ := m["hooks"].([]any)
			if !ok || len(inner) == 0 {
				kept = append(kept, item)
				continue
			}
			var rest []any
			for _, h := range inner {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if slug, ok := OrchHookSlugFor(ev.Event, cmd); ok && slug == ev.Slug {
					n++
					continue
				}
				rest = append(rest, h)
			}
			if len(rest) == 0 {
				continue
			}
			if len(rest) < len(inner) {
				c := make(map[string]any, len(m))
				for k, v := range m {
					c[k] = v
				}
				c["hooks"] = rest
				item = c
			}
			kept = append(kept, item)
		}
		if n == 0 {
			continue
		}
		removed = append(removed, ev.Event)
		if len(kept) == 0 {
			delete(hooks, ev.Event)
		} else {
			hooks[ev.Event] = kept
		}
	}
	if len(removed) == 0 {
		return ""
	}
	return fmt.Sprintf("hooks: removed orch's %s (memory spaces are off)", strings.Join(removed, ", "))
}

// applyToolHookSwitch is applyHooksWithEvents' events for harness with
// spaces' tool hooks taken out while spaces are off, and the note of what
// that removed from root.
func applyToolHookSwitch(root map[string]any, events eventMap, harness string) (eventMap, string) {
	if !SpacesOff() {
		return events, ""
	}
	return withoutToolHooks(events, harness), dropToolHooks(root, harness)
}

// joinSummary appends note, when there is one, to summary on its own line.
func joinSummary(summary, note string) string {
	if note == "" {
		return summary
	}
	return summary + "\n" + note
}
