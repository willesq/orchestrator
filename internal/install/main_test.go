package install

import (
	"os"
	"testing"

	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestMain points the plugin steps at a simulated claude CLI, so no test in
// this package ever runs the real one or reaches a real ~/.claude, and reads
// no real settings toggles.
func TestMain(m *testing.M) {
	safetyNetRunner = func(loc safetynet.Locations) safetynet.Runner {
		return fakeclaude.New(loc.ClaudeDir)
	}
	// No test reads the developer's own orch.db for toggles.
	loadToggles = func() (settings.Toggles, error) { return settings.Toggles{}, nil }
	loadSwitches = func() (spaces.Enforcement, error) { return spaces.DefaultEnforcement(), nil }
	// Reason: most tests exercise an install with memory spaces on; the
	// ones with spaces off (spaces_off_test.go) set it themselves.
	loadSpacesEnabled = func() (bool, error) { return true, nil }
	os.Exit(m.Run())
}
