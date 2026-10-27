package install

import (
	"os"
	"testing"

	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
)

// TestMain points the plugin steps at a simulated claude CLI, so no test in
// this package ever runs the real one or reaches a real ~/.claude.
func TestMain(m *testing.M) {
	safetyNetRunner = func(loc safetynet.Locations) safetynet.Runner {
		return fakeclaude.New(loc.ClaudeDir)
	}
	os.Exit(m.Run())
}
