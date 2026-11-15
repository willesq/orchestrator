package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/willesq/orchestrator/internal/paths"
)

// sessionState is what the prompt hook remembers across one session's turns.
type sessionState struct {
	// Injected holds hit keys already injected this session, oldest first, so a
	// repeated topic does not re-inject the same chunk turn after turn.
	Injected []string `json:"injected,omitempty"`
	// Seen counts, per event stream, the events this session has been shown.
	Seen map[string]int `json:"seen,omitempty"`
	// Sandbox is the sandbox setting this session's digest last named
	// (D-68); nil when none was.
	Sandbox *sandboxSeen `json:"sandbox,omitempty"`
}

// maxInjected bounds the remembered hits; a very long session forgets its
// oldest injections rather than growing the file without limit.
const maxInjected = 500

// sessionIDPattern is the shape a harness session ID must have before it names a
// file. The ID arrives in the hook payload, so it is validated rather than
// trusted with a path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func statePath(sessionID string) (string, bool) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", false
	}
	return filepath.Join(paths.SessionsDir(), sessionID+".hook.json"), true
}

// loadState returns the session's state, or an empty one when there is none or
// it cannot be read. The hook is best-effort and never blocks a prompt.
func loadState(sessionID string) sessionState {
	var st sessionState
	p, ok := statePath(sessionID)
	if !ok {
		return st
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// saveState writes the state atomically, owner-only: it records what this
// session was shown.
func saveState(sessionID string, st sessionState) error {
	p, ok := statePath(sessionID)
	if !ok {
		return nil
	}
	if n := len(st.Injected); n > maxInjected {
		st.Injected = st.Injected[n-maxInjected:]
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".hook-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// deleteState removes the session's state once the session has ended.
func deleteState(sessionID string) {
	if p, ok := statePath(sessionID); ok {
		_ = os.Remove(p)
	}
}

// hitKey identifies one chunk. It is hashed so the state file records which
// chunks were shown without copying their text.
func hitKey(uri, text string) string {
	sum := sha256.Sum256([]byte(uri + "\x00" + text))
	return hex.EncodeToString(sum[:12])
}
