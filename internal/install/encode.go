package install

import (
	"encoding/json"

	"github.com/pelletier/go-toml/v2"
)

// Every structured edit reads and writes through the encoders below. Sharing one
// encoder per format is what makes Edit.Normalize meaningful: the normalized form
// of a file on disk is byte-comparable with what Compute would write, so drift
// reflects the wiring rather than the harness's formatting habits.

// marshalJSON encodes a settings tree the way the orchestrator writes JSON.
func marshalJSON(root map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// normalizeJSON re-encodes an existing JSON file through marshalJSON. Empty input
// stays empty, so a missing file still reads as needing creation.
func normalizeJSON(old []byte) ([]byte, error) {
	if len(old) == 0 {
		return nil, nil
	}
	root := map[string]any{}
	if err := json.Unmarshal(old, &root); err != nil {
		return nil, err
	}
	return marshalJSON(root)
}

// marshalTOML encodes a settings tree the way the orchestrator writes TOML.
func marshalTOML(root map[string]any) ([]byte, error) {
	return toml.Marshal(root)
}

// normalizeTOML re-encodes an existing TOML file through marshalTOML.
func normalizeTOML(old []byte) ([]byte, error) {
	if len(old) == 0 {
		return nil, nil
	}
	root := map[string]any{}
	if err := toml.Unmarshal(old, &root); err != nil {
		return nil, err
	}
	return marshalTOML(root)
}
