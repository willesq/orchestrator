package cli

import (
	"os"
	"testing"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
	orchsettings "github.com/willesq/orchestrator/internal/settings"
)

// TestMain points install's plugin steps at a simulated claude CLI, so no
// test here ever runs the real one.
func TestMain(m *testing.M) {
	install.SetSafetyNetRunner(func(loc safetynet.Locations) safetynet.Runner {
		return fakeclaude.New(loc.ClaudeDir)
	})
	install.SetToggleLoader(func() (orchsettings.Toggles, error) { return orchsettings.Toggles{}, nil })
	os.Exit(m.Run())
}
