package safetynet

import (
	"fmt"
	"strings"
)

// Summary renders the state as a short value and a detail line naming the
// fix, for `orch status`.
func (s State) Summary() (value, detail string) {
	const fix = "run: orch install --apply, or Install in Orch.app"
	var parts []string
	value = string(s.Plugin)
	switch s.Plugin {
	case StateOK:
		parts = append(parts, fmt.Sprintf("%s v%s", PluginName, s.Pinned.Version))
	case StateMissing:
		parts = append(parts, PluginName+" is not installed — "+fix)
	case StateLegacy:
		ids := make([]string, len(s.Legacy))
		for i, in := range s.Legacy {
			ids[i] = fmt.Sprintf("%s v%s", in.ID, in.Version)
		}
		parts = append(parts, strings.Join(ids, ", ")+" installed instead of the pinned v"+s.Pinned.Version+" — "+fix)
	case StateOutdated:
		installed := "?"
		if s.Installed != nil {
			installed = s.Installed.Version
		}
		parts = append(parts, fmt.Sprintf("v%s installed, v%s pinned — %s", installed, s.Pinned.Version, fix))
	case StateDisabled:
		parts = append(parts, "installed but disabled — enable with: claude plugin enable "+PluginID)
	}
	if p := s.Rules.Problems(); len(p) > 0 {
		if s.Plugin == StateOK {
			value = "rules incomplete"
		}
		parts = append(parts, "rules: "+strings.Join(p, "; ")+" — run: orch install --apply, or Repair rules in Orch.app")
	} else {
		parts = append(parts, RulebookName+" active")
	}
	parts = append(parts, s.Errors...)
	return value, strings.Join(parts, "; ")
}
