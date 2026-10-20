// Package handoff passes work from hooks to orchd through small files. Hooks
// must return within the time a harness allows, so slow work — embedding a
// transcript or a document, refreshing PR state — is written down here and done
// by orchd in the background. Being on disk, it survives orchd being down or
// restarting before it gets to it.
package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/session"
)

// Dir is where hand-offs wait.
func Dir() string { return filepath.Join(paths.DataDir(), "handoff") }

// Session is a finished session whose transcript still has to be stored.
type Session struct {
	ID             string   `json:"session_id"`
	TranscriptPath string   `json:"transcript_path"`
	Tags           []string `json:"tags,omitempty"`
	// Record asks orchd to store the session record too, because the hook
	// could not.
	Record bool `json:"record,omitempty"`
	// Summarize asks orchd to queue the session's summary, because the hook
	// could not.
	Summarize bool `json:"summarize,omitempty"`
	// Attempts counts failed tries, so work that cannot be done is eventually
	// given up on.
	Attempts int `json:"attempts,omitempty"`
	// Updated is when the entry was last written, as read from disk.
	Updated time.Time `json:"-"`
}

// File is an edited document to store again.
type File struct {
	Path     string    `json:"path"`
	Attempts int       `json:"attempts,omitempty"`
	Updated  time.Time `json:"-"`
}

// entry is what list needs from an entry type.
type entry interface {
	key() string
	setUpdated(time.Time)
}

func (s Session) key() string             { return s.ID }
func (s *Session) setUpdated(t time.Time) { s.Updated = t }
func (f File) key() string                { return fileKey(f.Path) }
func (f *File) setUpdated(t time.Time)    { f.Updated = t }

// PutSession saves s, replacing an earlier entry for the same session, so a
// resumed session that ends again is stored once.
func PutSession(dir string, s Session) error {
	if !session.ValidID(s.ID) {
		return session.ErrInvalidSessionID
	}
	return put(filepath.Join(dir, "sessions"), s.ID, s)
}

// Sessions returns the sessions handed off before cutoff. A newer entry may
// still be rewritten by the hook that created it, so it waits.
func Sessions(dir string, cutoff time.Time) ([]Session, error) {
	return list[Session](filepath.Join(dir, "sessions"), cutoff)
}

// DoneSession removes s's entry, unless it was rewritten since s was read:
// a session that ended again while orchd worked keeps its newer entry.
func DoneSession(dir string, s Session) error {
	if !session.ValidID(s.ID) {
		return session.ErrInvalidSessionID
	}
	return removeIfUnchanged(filepath.Join(dir, "sessions"), s.ID, s.Updated)
}

// RetrySession records a failed attempt at s, unless its entry was rewritten
// since s was read.
func RetrySession(dir string, s Session) error {
	if !unchanged(filepath.Join(dir, "sessions", s.ID+".json"), s.Updated) {
		return nil
	}
	s.Attempts++
	return PutSession(dir, s)
}

// FailSession moves s's entry aside after its last attempt, kept for inspection.
func FailSession(dir string, s Session) error {
	if !session.ValidID(s.ID) {
		return session.ErrInvalidSessionID
	}
	return moveToFailed(dir, "sessions", s.ID)
}

// PutFile saves an edited document for orchd to store, replacing an earlier
// entry for the same path, so a burst of edits is stored once.
func PutFile(dir string, f File) error { return put(filepath.Join(dir, "files"), fileKey(f.Path), f) }

// Files returns the documents handed off before cutoff.
func Files(dir string, cutoff time.Time) ([]File, error) {
	return list[File](filepath.Join(dir, "files"), cutoff)
}

// DoneFile removes f's entry, unless the document was edited again since.
func DoneFile(dir string, f File) error {
	return removeIfUnchanged(filepath.Join(dir, "files"), fileKey(f.Path), f.Updated)
}

// RetryFile records a failed attempt at f, unless it was edited again since.
func RetryFile(dir string, f File) error {
	if !unchanged(filepath.Join(dir, "files", fileKey(f.Path)+".json"), f.Updated) {
		return nil
	}
	f.Attempts++
	return PutFile(dir, f)
}

// FailFile moves f's entry aside after its last attempt.
func FailFile(dir string, f File) error { return moveToFailed(dir, "files", fileKey(f.Path)) }

func fileKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:16])
}

// RequestPRSweep asks orchd to refresh PR state soon.
func RequestPRSweep(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "pr-sweep"), os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// TakePRSweep reports whether a PR refresh was requested, and clears the request.
func TakePRSweep(dir string) bool {
	return os.Remove(filepath.Join(dir, "pr-sweep")) == nil
}

// Classify decides whether an edited file is stored, and as what: files under
// ~/.claude as native config, other markdown as documentation. Only clean
// absolute paths qualify, so ~/.claude/../.ssh/x is not native config.
func Classify(path, home string) (sourceType, uri string, ok bool) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", false
	}
	if claudeDir := home + "/.claude/"; home != "" && strings.HasPrefix(path, claudeDir) {
		return "claude_config", "claude://" + strings.TrimPrefix(path, claudeDir), true
	}
	if strings.HasSuffix(strings.ToLower(path), ".md") {
		return "repo_doc", "file://" + path, true
	}
	return "", "", false
}

// staleTemp is how old a leftover temporary file must be before it is removed.
const staleTemp = time.Hour

func put(dir, key string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Written whole to a unique temporary file and renamed into place, so orchd
	// never reads half an entry and concurrent writers never share a file.
	f, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, key+".json"))
}

func list[T any, P interface {
	*T
	entry
}](dir string, cutoff time.Time) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []T
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil || !e.Type().IsRegular() {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			if time.Since(info.ModTime()) > staleTemp {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") || info.ModTime().After(cutoff) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var v T
		// An entry whose name and content disagree was not written by put;
		// it is dropped rather than trusted.
		if json.Unmarshal(b, &v) != nil || P(&v).key()+".json" != name {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		P(&v).setUpdated(info.ModTime())
		out = append(out, v)
	}
	return out, nil
}

// unchanged reports whether the entry at path still has the modification time
// it had when read.
func unchanged(path string, updated time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && info.ModTime().Equal(updated)
}

func removeIfUnchanged(dir, key string, updated time.Time) error {
	path := filepath.Join(dir, key+".json")
	if !unchanged(path, updated) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func moveToFailed(dir, kind, key string) error {
	failed := filepath.Join(dir, "failed", kind)
	if err := os.MkdirAll(failed, 0o700); err != nil {
		return err
	}
	err := os.Rename(filepath.Join(dir, kind, key+".json"), filepath.Join(failed, key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
