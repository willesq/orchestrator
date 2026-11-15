package hook

import (
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Memory spaces are opt-in (spaces.enabled, off by default). Off, the hooks
// do what they did before spaces: pre-tool-use and cwd-changed do nothing,
// session-start pins nothing and adds no enforcement lines, and session-end
// tells orchd nothing. Hooks a spaces install wrote stay harmless until the
// next `orch install --apply` removes them.

// spacesOff reports whether memory spaces are switched off; tests replace
// it.
var spacesOff = readSpacesOff

// readSpacesOff reads the switch from orchd's guard snapshot, else orch.db.
func readSpacesOff() bool { return spacesOffFrom(paths.GuardDir(), config.SpacesEnabled) }

// spacesOffFrom is spaces.SwitchedOff.
func spacesOffFrom(guardDir string, enabled func() (bool, error)) bool {
	return spaces.SwitchedOff(guardDir, enabled)
}
