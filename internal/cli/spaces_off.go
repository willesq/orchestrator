package cli

import (
	"errors"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// cliSpacesOff reports whether memory spaces are switched off; tests
// replace it.
var cliSpacesOff = spacesSwitchedOff

// spacesSwitchedOff reads the switch as the hooks do: from orchd's guard
// snapshot, else orch.db.
func spacesSwitchedOff() bool { return spaces.SwitchedOff(paths.GuardDir(), config.SpacesEnabled) }

// errCLISpacesOff refuses what only works with memory spaces on.
var errCLISpacesOff = errors.New("memory spaces are off; turn them on first with 'orch spaces on' (or Settings > Spaces in Orch.app)")
