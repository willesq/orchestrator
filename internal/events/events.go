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
	"regexp"
	"syscall"
)

// streamName is what a stream may be called. Streams are named by session IDs
// from harness payloads, so a name must never be able to reach outside dir.
var streamName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ErrInvalidStream rejects a stream name that cannot safely name a file.
var ErrInvalidStream = errors.New("invalid event stream name")

// ErrNotRegular rejects a stream path that is not a regular file.
var ErrNotRegular = errors.New("event stream is not a regular file")

// Event is one entry in a stream.
type Event struct {
	Kind      string `json:"kind"` // complete | blocked | failed | info | refusal
	Source    string `json:"source"`
	TaskID    string `json:"task_id,omitempty"`
	Summary   string `json:"summary"`
	Timestamp string `json:"timestamp"` // RFC3339
	// Refusal is set on a refusal event (KindRefusal) only.
	Refusal *Refusal `json:"refusal,omitempty"`
	// Space is the space of what the event reports, set by orchd when the
	// event is derived from content placed in one (INV-09): a finished
	// queued task carries the space its dispatch recorded. Empty with no
	// spaces policy, and on events that predate spaces.
	Space string `json:"space,omitempty"`
}

// Append writes one event to <dir>/<stream>.jsonl, creating the dir/file as needed.
func Append(dir, stream string, e Event) error {
	return appendMode(dir, stream, e, 0o644)
}

// appendMode is Append creating the file with perm. With an owner-only
// perm it also narrows an existing file to it.
func appendMode(dir, stream string, e Event, perm os.FileMode) error {
	path, err := streamPath(dir, stream)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Reason: a FIFO or a link planted at the path must not block the
	// writer (orch-hook writes refusals while the harness waits) or send
	// the event elsewhere.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return ErrNotRegular
	}
	if perm&0o077 == 0 {
		if err := f.Chmod(perm); err != nil {
			return err
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns all events in a stream (empty if the stream does not exist).
func Read(dir, stream string) ([]Event, error) {
	path, err := streamPath(dir, stream)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
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

func streamPath(dir, stream string) (string, error) {
	if !streamName.MatchString(stream) {
		return "", ErrInvalidStream
	}
	return filepath.Join(dir, stream+".jsonl"), nil
}
