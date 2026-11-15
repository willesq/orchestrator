package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is what the connector remembers between runs: a watermark per
// container (the last change it stored) and the content hash of each
// conversation it stored, by URI, so an unchanged conversation is not
// written again. It holds no message text and no credential.
type State struct {
	path string
	// Version is the file format.
	Version int `json:"version"`
	// Watermarks maps a container's URI to the newest change stored.
	Watermarks map[string]time.Time `json:"watermarks"`
	// Hashes maps a conversation's URI to the hash of what was stored.
	Hashes map[string]string `json:"hashes"`
	// AtMark maps a chat's URI to the IDs of its messages last changed at
	// its watermark's millisecond, which a later run asks for again (a
	// message can change in the same millisecond after a run) and so must
	// tell from new ones.
	AtMark map[string][]string `json:"at_mark,omitempty"`
}

const stateVersion = 1

// NewState is an empty state kept in memory only.
func NewState() *State {
	return &State{Version: stateVersion, Watermarks: map[string]time.Time{}, Hashes: map[string]string{}, AtMark: map[string][]string{}}
}

// OpenState reads the state at path; a missing file is an empty state.
// Save writes it back there.
func OpenState(path string) (*State, error) {
	s := NewState()
	s.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("teams: read state: %w", err)
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("teams: state %s is not valid: %w", path, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("teams: state %s has version %d, want %d", path, s.Version, stateVersion)
	}
	if s.Watermarks == nil {
		s.Watermarks = map[string]time.Time{}
	}
	if s.Hashes == nil {
		s.Hashes = map[string]string{}
	}
	if s.AtMark == nil {
		s.AtMark = map[string][]string{}
	}
	return s, nil
}

// Save writes the state to its file, replacing it whole, readable by the
// user alone. A state from NewState is not saved.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".teams-state-*")
	if err != nil {
		return fmt.Errorf("teams: save state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("teams: save state: %w", err)
	}
	// Reason: a crash after the rename must not leave an empty state,
	// which would stop every later run from opening it.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("teams: save state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("teams: save state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("teams: save state: %w", err)
	}
	return nil
}

// forget drops what is remembered about the container at uri, so that if
// the operator allows it again its next run reads it from the start rather
// than missing what was said while it was not allowed.
func (s *State) forget(uri string) bool {
	_, had := s.Watermarks[uri]
	delete(s.Watermarks, uri)
	delete(s.AtMark, uri)
	for k := range s.Hashes {
		if strings.HasPrefix(k, uri+"/") || k == uri {
			delete(s.Hashes, k)
			had = true
		}
	}
	return had
}
