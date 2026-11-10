package toollog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Retention limits (SP-015, D-05).
const (
	// DefaultRetention is how long an ended chain's records are kept.
	DefaultRetention = 7 * 24 * time.Hour
	// RefusalRetention is how long a refused call's record is kept, counted
	// from when it was taken; never less than the chain's own retention.
	RefusalRetention = 90 * 24 * time.Hour
	// DefaultSessionCap caps the records of a chain whose owner still runs.
	DefaultSessionCap int64 = 50 << 20
	// DefaultTotalCap caps the log file's pages in use.
	DefaultTotalCap int64 = 500 << 20
)

// RetentionDays are the retention periods an operator may choose.
var RetentionDays = []int{3, 7, 30}

// recordSize estimates a record's stored bytes: its text and a fixed
// allowance for the integers and the row.
const recordSize = `(length(CAST(owner_id AS BLOB)) + length(CAST(kind AS BLOB)) +
	length(CAST(harness AS BLOB)) + length(CAST(event AS BLOB)) + length(CAST(tool AS BLOB)) +
	length(CAST(claimed_session AS BLOB)) + length(CAST(tool_use_id AS BLOB)) +
	length(CAST(agent_id AS BLOB)) + length(CAST(agent_type AS BLOB)) + length(CAST(cwd AS BLOB)) +
	length(CAST(old_cwd AS BLOB)) + length(CAST(new_cwd AS BLOB)) + length(CAST(targets AS BLOB)) +
	length(CAST(command AS BLOB)) + length(CAST(decision AS BLOB)) + length(CAST(rule AS BLOB)) +
	length(prev_hash) + length(hash) + 64)`

// Policy is what a sweep keeps.
type Policy struct {
	// Retention is how long an ended chain's records other than refusals
	// are kept after it ended.
	Retention time.Duration
	// SessionCap caps the records of a chain whose owner still runs; the
	// oldest records other than refusals go first.
	SessionCap int64
	// TotalCap caps the file's pages in use; the chains that ended first
	// lose their records other than recent refusals until it fits. Chains
	// whose owner runs are never cut for it.
	TotalCap int64
}

// DefaultPolicy is the policy with every default.
func DefaultPolicy() Policy {
	return Policy{Retention: DefaultRetention, SessionCap: DefaultSessionCap, TotalCap: DefaultTotalCap}
}

// ChainState is whether a chain's owner still runs and, if not, when it
// ended.
type ChainState struct {
	Live bool
	// Ended is when the owner ended; zero means at its chain's last record.
	Ended time.Time
}

// StateFunc reports a chain owner's state. An error leaves the chain
// untouched by this sweep.
type StateFunc func(context.Context, Owner) (ChainState, error)

// Cut is one chain a sweep cut: its records up to Seq (the pruned head
// after the cut, whose hash is Hash) may be gone.
type Cut struct {
	Owner   Owner
	Seq     int64
	Hash    string
	Deleted int64
	Reason  string // "trimmed", "expired" or "capped"
}

// SweepResult counts what a sweep deleted.
type SweepResult struct {
	Records int64 // records deleted
	Trimmed int   // live chains cut to SessionCap
	Expired int   // ended chains past their retention
	Capped  int   // ended chains cut for TotalCap
	// Cuts lists every cut, so orchd can log it outside the file.
	Cuts []Cut
	// Used is the file's bytes in use after the sweep; OverCap is set when
	// it is still over TotalCap with nothing left a sweep may delete.
	Used    int64
	OverCap bool
}

type chainInfo struct {
	owner Owner
	max   int64 // the chain's highest seq when the sweep began
	last  int64 // ts of the chain's last record
	size  int64
	ended time.Time
}

// Sweep applies p at time now. Ended chains lose their records other than
// refusals once Retention has passed since they ended, and refusals once
// RefusalRetention has also passed since each was taken; a chain whose
// owner runs is never expired, only cut to SessionCap. Then, while the file
// is over TotalCap, the chains that ended first lose what an expired chain
// loses, early; refusals younger than RefusalRetention are never deleted, so
// the file can stay over the cap. A sweep never deletes past the records a
// chain had when the sweep read its owner's state, so records written
// meanwhile are kept. A chain's last record before the cut is kept as its
// pruned head, so its chain still verifies and later records still link
// (see Verify); anchored heads are kept. Sweeping again with the same now
// deletes nothing. A chain that cannot be read or cut is left for the next
// sweep, and its error is returned with the others once the rest are done.
func (s *Store) Sweep(ctx context.Context, now time.Time, p Policy, state StateFunc) (SweepResult, error) {
	var res SweepResult
	chains, err := s.chains(ctx)
	if err != nil {
		return res, err
	}
	var errs []error
	refusedBefore := now.Add(-RefusalRetention).UnixNano()
	cut := func(c chainInfo, reason string, fn func() (Cut, error)) bool {
		k, err := fn()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.owner, err))
			return false
		}
		if k.Deleted == 0 {
			return false
		}
		k.Reason = reason
		res.Cuts = append(res.Cuts, k)
		res.Records += k.Deleted
		return true
	}
	var ended []chainInfo
	for _, c := range chains {
		st, err := state(ctx, c.owner)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: state: %w", c.owner, err))
			continue
		}
		if st.Live {
			if p.SessionCap > 0 && c.size > p.SessionCap &&
				cut(c, "trimmed", func() (Cut, error) { return s.trim(ctx, c, p.SessionCap) }) {
				res.Trimmed++
			}
			continue
		}
		c.ended = time.Unix(0, c.last)
		if st.Ended.After(c.ended) {
			c.ended = st.Ended
		}
		if now.Before(c.ended.Add(p.Retention)) {
			ended = append(ended, c)
			continue
		}
		if cut(c, "expired", func() (Cut, error) { return s.expire(ctx, c, refusedBefore) }) {
			res.Expired++
		}
	}
	if res.Used, err = s.used(ctx); err != nil {
		return res, errors.Join(append(errs, err)...)
	}
	sort.SliceStable(ended, func(i, j int) bool { return ended[i].ended.Before(ended[j].ended) })
	for _, c := range ended {
		if p.TotalCap <= 0 || res.Used <= p.TotalCap {
			break
		}
		if cut(c, "capped", func() (Cut, error) { return s.expire(ctx, c, refusedBefore) }) {
			res.Capped++
			if res.Used, err = s.used(ctx); err != nil {
				return res, errors.Join(append(errs, err)...)
			}
		}
	}
	res.OverCap = p.TotalCap > 0 && res.Used > p.TotalCap
	if res.Records > 0 {
		if err := s.vacuum(ctx); err != nil {
			errs = append(errs, fmt.Errorf("vacuum: %w", err))
		}
	}
	return res, errors.Join(errs...)
}

// vacuumStep is how many free pages one vacuum step returns, so the write
// lock is held briefly however much a sweep freed.
const vacuumStep = 2048

// vacuum gives free pages back to the file system: they are reused but never
// returned otherwise. Each step takes its turn with the writer.
// journal_size_limit (Open) bounds the -wal file the moves pass through.
func (s *Store) vacuum(ctx context.Context) error {
	last := int64(-1)
	for {
		var free int64
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		// A file without auto_vacuum (its conversion failed) never
		// frees any.
		if free == 0 || free == last {
			return nil
		}
		last = free
		if err := s.lockWrite(ctx); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumStep))
		s.unlockWrite()
		if err != nil {
			return err
		}
	}
}

// chains lists every chain that has records, with its size.
func (s *Store) chains(ctx context.Context) ([]chainInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_kind, owner_id, MAX(seq), MAX(ts), SUM(`+recordSize+`)
		FROM events GROUP BY owner_kind, owner_id ORDER BY owner_kind, owner_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []chainInfo
	for rows.Next() {
		var c chainInfo
		if err := rows.Scan(&c.owner.Kind, &c.owner.ID, &c.max, &c.last, &c.size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// used is the file's bytes in pages that hold data.
func (s *Store) used(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}

// trim deletes a live chain's oldest records other than refusals until the
// rest fit in limit.
func (s *Store) trim(ctx context.Context, c chainInfo, limit int64) (Cut, error) {
	var k Cut
	err := s.write(ctx, func(tx *sql.Tx) error {
		var total int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+recordSize+`), 0) FROM events
			WHERE owner_kind = ? AND owner_id = ? AND seq <= ?`, c.owner.Kind, c.owner.ID, c.max).Scan(&total); err != nil {
			return err
		}
		at, err := trimPoint(ctx, tx, c, total-limit)
		if err != nil || at == 0 {
			return err
		}
		k, err = prune(ctx, tx, c.owner, at, `decision != ?`, DecisionRefused)
		return err
	})
	return k, err
}

// trimPoint is the sequence number up to which a chain's oldest records
// other than refusals add up to at least over bytes; 0 when over <= 0.
func trimPoint(ctx context.Context, tx *sql.Tx, c chainInfo, over int64) (int64, error) {
	if over <= 0 {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, `+recordSize+` FROM events
		WHERE owner_kind = ? AND owner_id = ? AND decision != ? AND seq <= ? ORDER BY seq`,
		c.owner.Kind, c.owner.ID, DecisionRefused, c.max)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var seq, freed int64
	for freed < over && rows.Next() {
		var size int64
		if err := rows.Scan(&seq, &size); err != nil {
			return 0, err
		}
		freed += size
	}
	return seq, rows.Err()
}

// expire deletes an ended chain's records other than refusals, and its
// refusals taken at or before refusedBefore (unix nanoseconds), up to the
// chain's last record when the sweep read it.
func (s *Store) expire(ctx context.Context, c chainInfo, refusedBefore int64) (Cut, error) {
	var k Cut
	err := s.inTx(ctx, func(tx execer) error {
		var err error
		k, err = prune(ctx, tx, c.owner, c.max, `(decision != ? OR ts <= ?)`, DecisionRefused, refusedBefore)
		return err
	})
	return k, err
}

// prune deletes the chain's records up to sequence number at that match
// where (with args), and moves the chain's pruned head up to at, keeping
// the record at's hash and kind there.
func prune(ctx context.Context, q execer, o Owner, at int64, where string, args ...any) (Cut, error) {
	k := Cut{Owner: o, Seq: at}
	var kind string
	if err := q.QueryRowContext(ctx, `SELECT hash, kind FROM events
		WHERE owner_kind = ? AND owner_id = ? AND seq = ?`, o.Kind, o.ID, at).Scan(&k.Hash, &kind); err != nil {
		return Cut{}, err
	}
	res, err := q.ExecContext(ctx, `DELETE FROM events WHERE owner_kind = ? AND owner_id = ? AND seq <= ? AND `+where,
		append([]any{o.Kind, o.ID, at}, args...)...)
	if err != nil {
		return Cut{}, err
	}
	if k.Deleted, err = res.RowsAffected(); err != nil || k.Deleted == 0 {
		return k, err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO pruned (owner_kind, owner_id, seq, hash, kind) VALUES (?,?,?,?,?)
		ON CONFLICT (owner_kind, owner_id) DO UPDATE SET seq = excluded.seq, hash = excluded.hash, kind = excluded.kind
		WHERE excluded.seq > pruned.seq`, o.Kind, o.ID, at, k.Hash, kind); err != nil {
		return Cut{}, err
	}
	// A later cut of refusals below the head leaves the head where it was;
	// the cut reports the head as it now is.
	h, err := prunedHead(ctx, q, o)
	k.Seq, k.Hash = h.seq, h.hash
	return k, err
}

// prunedHead is how far retention has cut the owner's chain: records up to
// its seq may be gone, and its hash is the record at seq's. The zero head
// when nothing was cut.
func prunedHead(ctx context.Context, q execer, o Owner) (chainHead, error) {
	var h chainHead
	err := q.QueryRowContext(ctx, `SELECT seq, hash, kind FROM pruned WHERE owner_kind = ? AND owner_id = ?`,
		o.Kind, o.ID).Scan(&h.seq, &h.hash, &h.kind)
	if errors.Is(err, sql.ErrNoRows) {
		return chainHead{}, nil
	}
	return h, err
}
