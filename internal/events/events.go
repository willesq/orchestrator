// Package events is an append-only JSONL event log keyed by stream (a session ID
// or a shared stream like "pr-babysitter"). Subagent completions and PR-state
// transitions are appended here and surfaced to the parent session by the hooks.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Event is one entry in a stream.
type Event struct {
	Kind      string `json:"kind"` // complete | blocked | failed | info
	Source    string `json:"source"`
	TaskID    string `json:"task_id,omitempty"`
	Summary   string `json:"summary"`
	Timestamp string `json:"timestamp"` // RFC3339
}

// Append writes one event to <dir>/<stream>.jsonl, creating the dir/file as needed.
func Append(dir, stream string, e Event) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(streamPath(dir, stream), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns all events in a stream (empty if the stream does not exist).
func Read(dir, stream string) ([]Event, error) {
	f, err := os.Open(streamPath(dir, stream))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed lines rather than fail the read
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func streamPath(dir, stream string) string {
	return filepath.Join(dir, stream+".jsonl")
}
