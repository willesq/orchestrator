package toollog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sort"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)
)

// fileMode is the command log's mode, and its -wal and -shm files' (D-29).
const fileMode fs.FileMode = 0o600

// schemaVersion 3 adds events.rules_rev (spaces D-66), added in place to
// a log of an older version.
const schemaVersion = 3

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
	rules_rev       INTEGER NOT NULL DEFAULT 0,
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
	command, input_size, decision, rule, policy_rev, rules_rev, dropped, prev_hash, hash`

// Store is the command log database.
type Store struct {
	db   *sql.DB
	path string
	// wsem serialises orchd's write transactions on the log (see write);
	// a channel so a caller can stop waiting when its context ends.
	wsem chan struct{}
	// onCut, when set, is given each retention cut once it is committed
	// (SetCutHook).
	onCut func(Cut)
}

// SetCutHook makes every later retention cut be handed to fn once it is
// committed, so orchd can copy the pruned head into orch.db (D-35). Set it
// before the first Sweep; fn runs on the sweeping goroutine.
func (s *Store) SetCutHook(fn func(Cut)) { s.onCut = fn }

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
	if err := addRulesRev(ctx, conn); err != nil {
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

// addRulesRev adds events.rules_rev to a log made before schema 3. Reason:
// CREATE TABLE IF NOT EXISTS leaves an existing table as it was. Records
// stored before it read as revision 0, the baseline, and keep their hashes,
// since a record's JSON leaves a zero revision out.
func addRulesRev(ctx context.Context, conn *sql.Conn) error {
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('events') WHERE name = 'rules_rev'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := conn.ExecContext(ctx, `ALTER TABLE events ADD COLUMN rules_rev INTEGER NOT NULL DEFAULT 0`)
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.OwnerKind, r.OwnerID, r.Seq, r.TS, r.HookAt, r.Kind, r.Harness, r.Event, r.Tool,
		r.ClaimedSession, r.ToolUseID, r.AgentID, r.AgentType, r.Cwd, r.OldCwd, r.NewCwd, r.Targets,
		r.Command, r.InputSize, r.Decision, r.Rule, r.PolicyRev, r.RulesRev, r.Dropped, r.PrevHash, r.Hash)
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
	return scanRecords(rows)
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
	Pruned int64
	// Copies counts the anchors orch.db holds for the chain, when they
	// were compared (VerifyCopies).
	Copies   int
	Problems []Problem
}

// OK reports whether the chain is intact.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify recomputes the owner's chain: each record's hash, its link to the
// one before, unbroken sequence numbers from 1, and every anchored head.
// A modified record fails its hash, a deleted one leaves a gap in the
// sequence, and a moved one fails its hash or its link. Records removed
// after the last anchor, or a chain rewritten from its first record, are
// not detectable from the file alone (VerifyCopies compares orch.db's
// copies of the anchors); see docs/internals/daemon.md.
//
// Up to the chain's pruned head, retention may have deleted records: there
// a missing record is no problem, and a record links only to one kept just
// before it. Retention leaves only refusals there, so any other record kept
// at or before the head is reported, as is a head whose own record is kept
// with another hash. The first record after the pruned head must link to it.
func (s *Store) Verify(ctx context.Context, o Owner) (Report, error) {
	return s.verify(ctx, o, nil)
}

// Copies is what orch.db holds of one owner's chain (D-35): its anchors, and
// the pruned head of retention's last cut (a zero Cut when none).
type Copies struct {
	Anchors []Anchor
	Cut     Cut
}

// VerifyCopies verifies the owner's chain as Verify does and compares it
// with copies, the owner's anchors and pruned head as orch.db holds them
// (D-35). The copies catch what the file alone cannot: a chain whose
// records after its last anchor in the file were removed, with that anchor,
// or that was rewritten from its first record, disagrees with them. An
// anchor in only one of the two stores is reported, and so is a pruned
// head that differs from orch.db's; the chain is then checked against
// orch.db's, so records deleted under a pruned head written into the file
// by hand are reported missing.
func (s *Store) VerifyCopies(ctx context.Context, o Owner, copies Copies) (Report, error) {
	return s.verify(ctx, o, &copies)
}

func (s *Store) verify(ctx context.Context, o Owner, copies *Copies) (Report, error) {
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
	if copies != nil {
		if trusted := (chainHead{seq: copies.Cut.Seq, hash: copies.Cut.Hash}); cut.seq != trusted.seq || cut.hash != trusted.hash {
			at := max(cut.seq, trusted.seq)
			rep.add(at, fmt.Sprintf("the pruned head in toollog.db (record %d) does not match orch.db's copy (record %d): retention did not cut it", cut.seq, trusted.seq))
			cut = trusted
		}
	}
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
	inFile := map[int64]string{}
	for rows.Next() {
		var seq int64
		var hash string
		if err := rows.Scan(&seq, &hash); err != nil {
			return Report{}, err
		}
		rep.Anchors++
		inFile[seq] = hash
		rep.checkHead(seq, hash, bySeq, cut, "does not match the chain head anchored at its end")
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	if copies != nil {
		rep.compareCopies(inFile, copies.Anchors, bySeq, cut)
	}
	return rep, nil
}

// checkHead reports an anchored head at seq whose record is missing (and
// not deleted by retention) or has another hash than hash.
func (r *Report) checkHead(seq int64, hash string, bySeq map[int64]string, cut chainHead, mismatch string) {
	switch got, ok := bySeq[seq]; {
	case !ok && seq < cut.seq:
		// Deleted by retention; the anchor is kept.
	case !ok && seq == cut.seq:
		// Retention keeps the deleted head's hash.
		if cut.hash != hash {
			r.add(seq, mismatch)
		}
	case !ok:
		r.add(seq, "the anchored chain head is missing (records after it were removed)")
	case got != hash:
		r.add(seq, mismatch)
	}
}

// compareCopies checks the file's anchors (seq to hash) against orch.db's
// copies, and the records against every copy.
func (r *Report) compareCopies(inFile map[int64]string, copies []Anchor, bySeq map[int64]string, cut chainHead) {
	seen := make(map[int64]bool, len(copies))
	for _, c := range copies {
		if seen[c.Seq] {
			continue
		}
		seen[c.Seq] = true
		r.Copies++
		h, ok := inFile[c.Seq]
		switch {
		case !ok:
			r.add(c.Seq, "anchored in orch.db but the anchor is missing from toollog.db")
		case h != c.Hash:
			r.add(c.Seq, "the anchors in toollog.db and orch.db disagree (the chain was rewritten)")
		}
		if !ok || h != c.Hash {
			// Reason: a missing or rewritten file anchor proves nothing
			// about the records; check them against the copy too.
			r.checkHead(c.Seq, c.Hash, bySeq, cut, "does not match the chain head anchored in orch.db (the chain was rewritten)")
		}
	}
	for seq := range inFile {
		if !seen[seq] {
			r.add(seq, "anchored in toollog.db but orch.db has no copy of the anchor")
		}
	}
	sort.SliceStable(r.Problems, func(i, j int) bool { return r.Problems[i].Seq < r.Problems[j].Seq })
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
