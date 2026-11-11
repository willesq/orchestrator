package toollog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Anchor is one anchored chain head: the hash of the owner's record at Seq,
// written when the chain was ended (TS is that end record's). orchd copies
// each into orch.db as well (D-35), so verify can compare the two stores.
type Anchor struct {
	Owner Owner
	Seq   int64
	Hash  string
	At    int64
}

// Anchors returns every anchor in the file, ordered by owner and sequence
// number.
func (s *Store) Anchors(ctx context.Context) ([]Anchor, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner_kind, owner_id, seq, hash, at FROM anchors ORDER BY owner_kind, owner_id, seq`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Anchor
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.Owner.Kind, &a.Owner.ID, &a.Seq, &a.Hash, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Cuts returns every chain's pruned head, as Cuts with no count or reason.
func (s *Store) Cuts(ctx context.Context) ([]Cut, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_kind, owner_id, seq, hash FROM pruned ORDER BY owner_kind, owner_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Cut
	for rows.Next() {
		var c Cut
		if err := rows.Scan(&c.Owner.Kind, &c.Owner.ID, &c.Seq, &c.Hash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenChains returns the chains of kind whose last record is not an end
// record: their owner has not been ended yet, or wrote again since.
func (s *Store) OpenChains(ctx context.Context, kind string) ([]Owner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.owner_id FROM events e
		JOIN (SELECT owner_id, MAX(seq) AS seq FROM events WHERE owner_kind = ? GROUP BY owner_id) m
		ON e.owner_id = m.owner_id AND e.seq = m.seq
		WHERE e.owner_kind = ? AND e.kind != ? ORDER BY e.owner_id`, kind, kind, KindEnd)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Owner
	for rows.Next() {
		o := Owner{Kind: kind}
		if err := rows.Scan(&o.ID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// LatestProcessChain is the chain of the process with this PID that took
// an event last; ok is false when that PID has none.
func (s *Store) LatestProcessChain(ctx context.Context, pid int) (o Owner, ok bool, err error) {
	o.Kind = OwnerProcess
	// Reason: a range over the primary key, which LIKE (case-insensitive)
	// cannot use; '.' sorts just after '-'.
	err = s.db.QueryRowContext(ctx, `SELECT owner_id FROM events
		WHERE owner_kind = ? AND owner_id >= ? AND owner_id < ? AND kind = ? ORDER BY ts DESC LIMIT 1`,
		OwnerProcess, fmt.Sprintf("%d-", pid), fmt.Sprintf("%d.", pid), KindEvent).Scan(&o.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Owner{}, false, nil
	}
	return o, err == nil, err
}

// ProcessChain is one unpinned harness process's chain, for the session
// list.
type ProcessChain struct {
	Owner    Owner
	Harness  string // the harness of its latest event
	Activity Activity
}

// ProcessChains lists every process chain with its activity.
func (s *Store) ProcessChains(ctx context.Context) ([]ProcessChain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id, COUNT(*), COALESCE(SUM(decision = ?), 0), MAX(ts),
		(SELECT harness FROM events h WHERE h.owner_kind = ? AND h.owner_id = e.owner_id AND h.kind = ?
			ORDER BY h.seq DESC LIMIT 1)
		FROM events e WHERE owner_kind = ? AND kind = ? GROUP BY owner_id ORDER BY owner_id`,
		DecisionRefused, OwnerProcess, KindEvent, OwnerProcess, KindEvent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessChain
	for rows.Next() {
		c := ProcessChain{Owner: Owner{Kind: OwnerProcess}}
		var harness sql.NullString
		if err := rows.Scan(&c.Owner.ID, &c.Activity.Events, &c.Activity.Refused, &c.Activity.Last, &harness); err != nil {
			return nil, err
		}
		c.Harness = harness.String
		out = append(out, c)
	}
	return out, rows.Err()
}
