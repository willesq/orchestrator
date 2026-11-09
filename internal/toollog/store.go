package toollog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)
)

// fileMode is the command log's mode, and its -wal and -shm files' (D-29).
const fileMode fs.FileMode = 0o600

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS events (
	owner_kind      TEXT    NOT NULL,
	owner_id        TEXT    NOT NULL,
	seq             INTEGER NOT NULL,
	ts              INTEGER NOT NULL,
	hook_at         INTEGER NOT NULL,
	kind            TEXT    NOT NULL,
	harness         TEXT    NOT NULL,
	event           TEXT    NOT NULL,
	tool            TEXT    NOT NULL,
	claimed_session TEXT    NOT NULL,
	tool_use_id     TEXT    NOT NULL,
	agent_id        TEXT    NOT NULL,
	agent_type      TEXT    NOT NULL,
	cwd             TEXT    NOT NULL,
	old_cwd         TEXT    NOT NULL,
	new_cwd         TEXT    NOT NULL,
	targets         TEXT    NOT NULL,
	command         TEXT    NOT NULL,
	input_size      INTEGER NOT NULL,
	decision        TEXT    NOT NULL,
	rule            TEXT    NOT NULL,
	policy_rev      INTEGER NOT NULL,
	dropped         INTEGER NOT NULL,
	prev_hash       TEXT    NOT NULL,
	hash            TEXT    NOT NULL,
	PRIMARY KEY (owner_kind, owner_id, seq)
);
CREATE INDEX IF NOT EXISTS events_ts ON events (ts);
CREATE TABLE IF NOT EXISTS anchors (
	owner_kind TEXT    NOT NULL,
	owner_id   TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	hash       TEXT    NOT NULL,
	at         INTEGER NOT NULL,
	PRIMARY KEY (owner_kind, owner_id, seq)
);`

const recordColumns = `owner_kind, owner_id, seq, ts, hook_at, kind, harness, event, tool,
	claimed_session, tool_use_id, agent_id, agent_type, cwd, old_cwd, new_cwd, targets,
	command, input_size, decision, rule, policy_rev, dropped, prev_hash, hash`

// Store is the command log database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens or creates the command log at path, owner-only. It refuses a
// path that is a symlink or not a regular file, so a planted link cannot
// send the log elsewhere.
func Open(path string) (*Store, error) {
	if err := ownerOnly(path, true); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// SQLite gives -wal and -shm the database's mode when it creates them;
	// files left by an older, wider database are tightened here.
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if err := ownerOnly(side, false); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// ownerOnly makes path a regular file of mode 0600, creating it when create
// is set; a missing file is otherwise left missing.
func ownerOnly(path string, create bool) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return nil
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
		if err != nil {
			return fmt.Errorf("command log: %w", err)
		}
		return f.Close()
	case err != nil:
		return fmt.Errorf("command log: %w", err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("command log: %s is not a regular file", path)
	case fi.Mode().Perm() != fileMode:
		return os.Chmod(path, fileMode)
	}
	return nil
}

func (s *Store) init() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("command log: schema %d is newer than this orchd (%d)", v, schemaVersion)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// head returns the owner's last record's sequence number, hash and kind;
// the zero head when the owner has no records.
func head(ctx context.Context, q execer, o Owner) (chainHead, error) {
	var h chainHead
	err := q.QueryRowContext(ctx,
		`SELECT seq, hash, kind FROM events WHERE owner_kind = ? AND owner_id = ? ORDER BY seq DESC LIMIT 1`,
		o.Kind, o.ID).Scan(&h.seq, &h.hash, &h.kind)
	if errors.Is(err, sql.ErrNoRows) {
		return chainHead{}, nil
	}
	return h, err
}

func insert(ctx context.Context, q execer, r Record) error {
	_, err := q.ExecContext(ctx, `INSERT INTO events (`+recordColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.OwnerKind, r.OwnerID, r.Seq, r.TS, r.HookAt, r.Kind, r.Harness, r.Event, r.Tool,
		r.ClaimedSession, r.ToolUseID, r.AgentID, r.AgentType, r.Cwd, r.OldCwd, r.NewCwd, r.Targets,
		r.Command, r.InputSize, r.Decision, r.Rule, r.PolicyRev, r.Dropped, r.PrevHash, r.Hash)
	return err
}

func anchor(ctx context.Context, q execer, r Record) error {
	_, err := q.ExecContext(ctx,
		`INSERT OR REPLACE INTO anchors (owner_kind, owner_id, seq, hash, at) VALUES (?,?,?,?,?)`,
		r.OwnerKind, r.OwnerID, r.Seq, r.Hash, r.TS)
	return err
}

// Records returns the owner's records in sequence order.
func (s *Store) Records(ctx context.Context, o Owner) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordColumns+` FROM events
		WHERE owner_kind = ? AND owner_id = ? ORDER BY seq`, o.Kind, o.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.OwnerKind, &r.OwnerID, &r.Seq, &r.TS, &r.HookAt, &r.Kind, &r.Harness,
			&r.Event, &r.Tool, &r.ClaimedSession, &r.ToolUseID, &r.AgentID, &r.AgentType, &r.Cwd,
			&r.OldCwd, &r.NewCwd, &r.Targets, &r.Command, &r.InputSize, &r.Decision, &r.Rule,
			&r.PolicyRev, &r.Dropped, &r.PrevHash, &r.Hash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Problem is one break in a chain.
type Problem struct {
	Seq    int64
	Detail string
}

// Report is the result of verifying one owner's chain.
type Report struct {
	Records int
	Anchors int
	// Ends counts KindEnd records; Gaps counts KindGap records and Dropped
	// the events they say were lost. Neither is a problem.
	Ends     int
	Gaps     int
	Dropped  int64
	Problems []Problem
}

// OK reports whether the chain is intact.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify recomputes the owner's chain: each record's hash, its link to the
// one before, unbroken sequence numbers from 1, and every anchored head.
// A modified record fails its hash, a deleted one leaves a gap in the
// sequence, and a moved one fails its hash or its link. Records removed
// after the last anchor, or a chain rewritten from its first record, are
// not detectable from the file alone; see docs/internals/daemon.md.
func (s *Store) Verify(ctx context.Context, o Owner) (Report, error) {
	recs, err := s.Records(ctx, o)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	rep.Records = len(recs)
	bySeq := make(map[int64]string, len(recs))
	var want int64 = 1
	prev := ""
	for _, r := range recs {
		switch {
		case r.Seq > want:
			rep.add(r.Seq, missing(want, r.Seq-1))
		case r.PrevHash != prev:
			rep.add(r.Seq, "does not link to the record before it (moved, or one before it changed)")
		}
		if got := r.computeHash(); got != r.Hash {
			rep.add(r.Seq, "content does not match its hash (modified or moved)")
		}
		switch r.Kind {
		case KindEnd:
			rep.Ends++
		case KindGap:
			rep.Gaps++
			rep.Dropped += r.Dropped
		}
		bySeq[r.Seq] = r.Hash
		want, prev = r.Seq+1, r.Hash
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, hash FROM anchors WHERE owner_kind = ? AND owner_id = ? ORDER BY seq`, o.Kind, o.ID)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seq int64
		var hash string
		if err := rows.Scan(&seq, &hash); err != nil {
			return Report{}, err
		}
		rep.Anchors++
		switch got, ok := bySeq[seq]; {
		case !ok:
			rep.add(seq, "the anchored chain head is missing")
		case got != hash:
			rep.add(seq, "does not match the chain head anchored at SessionEnd")
		}
	}
	return rep, rows.Err()
}

func (r *Report) add(seq int64, detail string) {
	r.Problems = append(r.Problems, Problem{Seq: seq, Detail: detail})
}

func missing(from, to int64) string {
	if from == to {
		return fmt.Sprintf("record %d before it is missing", from)
	}
	return fmt.Sprintf("records %d-%d before it are missing", from, to)
}
