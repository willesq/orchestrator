package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Files a session writes outside every folder root carry its space
// (spaces SP-029, INV-09). No root places such a file, so before this an
// edit hand-off of it went to quarantine. orchd now records, from each
// Write tool call it resolves to a pinned session or a tracked queued
// task, the path written when no root holds it, the writer's space and the
// hash of the text written. An edit hand-off of that path whose text has
// that hash is then placed in that space (quarantine when writers of two
// spaces wrote the same text).
//
// Reason for the hash: a tool event is a claim any caller can send for any
// path, before anything is written. Matching the text the call carried
// against the text ingested proves the file holds what that writer wrote,
// so a session cannot claim another space's file by naming its path (the
// SP-029 security review). Rows expire after sessionFilesKeep. See
// docs/internals/memory.md "Placement of writes".

// Bounds on the record. Variables so tests can shrink them.
var (
	// sessionFilesKeep is how long a write is remembered.
	sessionFilesKeep = 30 * 24 * time.Hour
	// sessionFilesMax caps the rows kept; the oldest go first.
	sessionFilesMax = 100_000
)

// sessionFilesTimeout bounds recording one call's write, which runs on the
// command log's writer goroutine.
const sessionFilesTimeout = 2 * time.Second

// sessionFiles is the session_files table in orch.db.
type sessionFiles struct {
	db  *sql.DB
	now func() time.Time
}

func newSessionFiles(db *sql.DB) *sessionFiles {
	if db == nil {
		return nil
	}
	return &sessionFiles{db: db, now: time.Now}
}

// fileKey is how a path is kept and looked up: cleaned and in lower case,
// as roots are matched, since macOS volumes are usually case-insensitive.
func fileKey(p string) string { return strings.ToLower(filepath.Clean(p)) }

// writtenText is the path and text of a Write tool call, from its targets
// (the path with symlinks resolved) and its input; ok is false for any
// other call, or one whose input does not carry the whole text.
func writtenText(tool string, input []byte, writes []string) (path, text string, ok bool) {
	if tool != "Write" || len(writes) != 1 {
		return "", "", false
	}
	var in struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(input, &in) != nil || in.Content == nil {
		return "", "", false
	}
	return writes[0], *in.Content, true
}

// writeHash is the hash a written text is recorded and matched by: that of
// the text as the edit hand-off stores it, redacted (storeFile).
func writeHash(text string) string {
	red, _ := redact.Redact(text)
	return rag.ContentHash(red)
}

// record notes that writer, in space, wrote text at path, when res holds
// path under no root. A relative path, and a space res does not have or
// that is public (sessions never write public, SP-030), record nothing.
func (f *sessionFiles) record(ctx context.Context, res *spaces.Resolved, space, writer, path, text string) {
	if f == nil || !res.Configured() || writer == "" || !filepath.IsAbs(path) || res.UnderRoot(path) {
		return
	}
	if sp, ok := res.Space(space); !ok || sp.Kind == spaces.Public {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sessionFilesTimeout)
	defer cancel()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO session_files (path, space, content_hash, writer, written_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path, space, content_hash) DO UPDATE SET writer = excluded.writer, written_at = excluded.written_at`,
		fileKey(path), space, writeHash(text), writer, f.now().UTC().Format(time.RFC3339Nano)); err != nil {
		log.Printf("orchd: recording a file %s wrote: %v", writer, err)
	}
}

// spacesOf lists the spaces whose writers wrote text hashed hash at path
// within sessionFilesKeep, sorted.
func (f *sessionFiles) spacesOf(ctx context.Context, path, hash string) ([]string, error) {
	if f == nil || hash == "" {
		return nil, nil
	}
	cutoff := f.now().Add(-sessionFilesKeep).UTC().Format(time.RFC3339Nano)
	rows, err := f.db.QueryContext(ctx, `SELECT space FROM session_files
		WHERE path = ? AND content_hash = ? AND written_at >= ?`, fileKey(path), hash, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var sp string
		if err := rows.Scan(&sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	slices.Sort(out)
	return out, rows.Err()
}

// prune deletes writes older than sessionFilesKeep and the oldest beyond
// sessionFilesMax. Reason: an evicted row only sends its file to
// quarantine, as before SP-029; it never lets another space's row win,
// since that row would have to match the file's text.
func (f *sessionFiles) prune(ctx context.Context) {
	if f == nil {
		return
	}
	cutoff := f.now().Add(-sessionFilesKeep).UTC().Format(time.RFC3339Nano)
	if _, err := f.db.ExecContext(ctx, `DELETE FROM session_files WHERE written_at < ?`, cutoff); err != nil {
		log.Printf("orchd: pruning recorded session files: %v", err)
		return
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM session_files WHERE rowid IN
		(SELECT rowid FROM session_files ORDER BY written_at DESC LIMIT -1 OFFSET ?)`, sessionFilesMax); err != nil {
		log.Printf("orchd: pruning recorded session files: %v", err)
	}
}

// fileWriter is who wrote through caller c, and the space it writes to:
// a session by its (trusted) pin, a tracked task by the space its dispatch
// recorded. Anyone else records nothing.
func fileWriter(c resolvedCaller) (writer, space string) {
	switch {
	case c.kind == callerSession && trustedPin(c.pin):
		return "session:" + c.pin.SessionID, c.pin.Space
	case c.kind == callerTask && c.task.TaskID != "":
		return taskPinOwner(c.task.TaskID), c.task.Space
	}
	return "", ""
}

// writtenPlacement places an edited file no root holds by the spaces of
// the writers whose recorded text is the text being stored (m.ContentHash):
// the one such space while res still has it and it is not public,
// quarantine for several or one res no longer allows, and ok false when
// none is recorded (the classifier's answer stands).
func (w *writePlacer) writtenPlacement(ctx context.Context, res *spaces.Resolved, m *rag.Meta) (spaces.Placement, bool, error) {
	sps, err := w.files.spacesOf(ctx, m.Origin.Path, m.ContentHash)
	switch {
	case err != nil:
		return spaces.Placement{}, false, err
	case len(sps) == 0:
		return spaces.Placement{}, false, nil
	case len(sps) > 1:
		return quarantined("sessions of more than one space wrote this text"), true, nil
	}
	// Reason: the row was written under an earlier policy, which may have
	// had a space this one lacks or makes public.
	if sp, ok := res.Space(sps[0]); !ok || sp.Kind == spaces.Public {
		return quarantined("the space that wrote it is not a private or isolated space of the applied policy"), true, nil
	}
	return spaces.Placement{Space: sps[0], Outcome: spaces.Placed, Rule: spaces.PlaceWriter,
		Reason: "a session of this space wrote this text here, outside every root"}, true, nil
}
