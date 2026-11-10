package toollog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)
)

// fileMode is the command log's mode, and its -wal and -shm files' (D-29).
const fileMode fs.FileMode = 0o600

const schemaVersion = 2

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
);
CREATE TABLE IF NOT EXISTS pruned (
	owner_kind TEXT    NOT NULL,
	owner_id   TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	hash       TEXT    NOT NULL,
	kind       TEXT    NOT NULL,
	PRIMARY KEY (owner_kind, owner_id)
);`

const recordColumns = `owner_kind, owner_id, seq, ts, hook_at, kind, harness, event, tool,
	claimed_session, tool_use_id, agent_id, agent_type, cwd, old_cwd, new_cwd, targets,
	command, input_size, decision, rule, policy_rev, dropped, prev_hash, hash`

// Store is the command log database.
type Store struct {
	db   *sql.DB
	path string
	// wsem serialises orchd's write transactions on the log (see write);
	// a channel so a caller can stop waiting when its context ends.
	wsem chan struct{}
}

// Open opens or creates the command log at path, owner-only. It refuses a
// path that is a symlink or not a regular file, so a planted link cannot
// send the log elsewhere.
func Open(path string) (*Store, error) {
	if err := ownerOnly(path, true); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=journal_size_limit(67108864)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path, wsem: make(chan struct{}, 1)}
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
	// Reason: auto_vacuum takes effect only on the connection that creates
	// the first table, and not in the connection string, where every new
	// connection would take the write lock to set it.
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var v int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("command log: schema %d is newer than this orchd (%d)", v, schemaVersion)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	var vacuum int
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&vacuum); err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	if vacuum != 2 {
		// Reason: a log created before retention (SP-015) has auto_vacuum
		// off, so deleted records would never shrink the file. VACUUM
		// needs the file to itself and room for a copy; a log that cannot
		// be converted still works, it only stays large.
		if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
			log.Printf("orchd: command log: converting to incremental vacuum: %v; deleted records will not shrink the file", err)
		}
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// write runs fn in a transaction that holds the write lock from its first
// statement, and commits it. The writer and retention both write the log
// from orchd: wsem makes them take turns, since SQLite's busy wait lets one
// that commits continuously starve the other. The lock is still taken
// first with a write that changes nothing: in WAL mode a transaction that
// reads first and writes later cannot wait for another writer, and fails
// at once (SQLITE_BUSY_SNAPSHOT).
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	defer s.unlockWrite()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pruned WHERE 0`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// lockWrite takes the turn to write, or gives up when ctx ends.
func (s *Store) lockWrite(ctx context.Context) error {
	select {
	case s.wsem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) unlockWrite() { <-s.wsem }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// head returns the owner's last record's sequence number, hash and kind,
// or retention's pruned head when that is later (the chain's records were
// deleted); the zero head when the owner has neither.
func head(ctx context.Context, q execer, o Owner) (chainHead, error) {
	var h chainHead
	err := q.QueryRowContext(ctx,
		`SELECT seq, hash, kind FROM events WHERE owner_kind = ? AND owner_id = ? ORDER BY seq DESC LIMIT 1`,
		o.Kind, o.ID).Scan(&h.seq, &h.hash, &h.kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return chainHead{}, err
	}
	p, err := prunedHead(ctx, q, o)
	if err != nil {
		return chainHead{}, err
	}
	if p.seq > h.seq {
		return p, nil
	}
	return h, nil
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return records(ctx, tx, o)
}

func records(ctx context.Context, tx *sql.Tx, o Owner) ([]Record, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM events
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

// Activity sums one session's chain for the session list (SP-017).
type Activity struct {
	Events  int64 // tool events (gap and end records excluded)
	Refused int64 // of those, calls the guard refused
	Last    int64 // when orchd took the latest event, unix nanoseconds
}

// SessionActivity returns the activity of each session in ids that has
// logged an event. Each lookup reads only that session's records.
func (s *Store) SessionActivity(ctx context.Context, ids []string) (map[string]Activity, error) {
	out := make(map[string]Activity, len(ids))
	for _, id := range ids {
		var a Activity
		var last sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(decision = ?), 0), MAX(ts)
			FROM events WHERE owner_kind = ? AND owner_id = ? AND kind = ?`,
			DecisionRefused, OwnerSession, id, KindEvent).Scan(&a.Events, &a.Refused, &last); err != nil {
			return nil, err
		}
		if a.Events > 0 {
			a.Last = last.Int64
			out[id] = a
		}
	}
	return out, nil
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
	Ends    int
	Gaps    int
	Dropped int64
	// Pruned is the sequence number up to which retention (SP-015) may
	// have deleted records; 0 when it has deleted none.
	Pruned   int64
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
//
// Up to the chain's pruned head, retention may have deleted records: there
// a missing record is no problem, and a record links only to one kept just
// before it. Retention leaves only refusals there, so any other record kept
// at or before the head is reported, as is a head whose own record is kept
// with another hash. The first record after the pruned head must link to it.
func (s *Store) Verify(ctx context.Context, o Owner) (Report, error) {
	// One read transaction, so a cut committed meanwhile is seen whole or
	// not at all.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = tx.Rollback() }()
	recs, err := records(ctx, tx, o)
	if err != nil {
		return Report{}, err
	}
	cut, err := prunedHead(ctx, tx, o)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	rep.Records = len(recs)
	rep.Pruned = cut.seq
	bySeq := make(map[int64]string, len(recs))
	var want int64 = 1
	prev := ""
	for _, r := range recs {
		if r.Seq > cut.seq && want <= cut.seq {
			// The first record past the cut follows the pruned head.
			want, prev = cut.seq+1, cut.hash
		}
		switch {
		case r.Seq <= cut.seq && (r.Kind != KindEvent || r.Decision != DecisionRefused):
			rep.add(r.Seq, "kept before the pruned head, where retention keeps only refusals")
		case r.Seq == cut.seq && r.Hash != cut.hash:
			rep.add(r.Seq, "does not match the pruned head retention recorded")
		case r.Seq <= cut.seq && r.Seq != want:
			// Records before it were deleted by retention.
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
	rows, err := tx.QueryContext(ctx,
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
		case !ok && seq <= cut.seq:
			// Deleted by retention; the anchor is kept.
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
