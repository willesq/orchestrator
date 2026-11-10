package toollog

import (
	"context"
	"database/sql"
	"errors"
)

// Event names the hooks report.
const (
	EventPreToolUse = "pre-tool-use"
	EventCwdChanged = "cwd-changed"
)

// Page returns the owner's records after sequence number after, in order:
// up to limit of them, and fewer once they hold more than maxBytes of text
// (always at least one). more says records remain past the last returned.
// pruned is the sequence number up to which retention may have deleted
// records (0 for none).
func (s *Store) Page(ctx context.Context, o Owner, after int64, limit, maxBytes int) (recs []Record, more bool, pruned int64, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM events
		WHERE owner_kind = ? AND owner_id = ? AND seq > ? ORDER BY seq LIMIT ?`, o.Kind, o.ID, after, limit+1)
	if err != nil {
		return nil, false, 0, err
	}
	n, size := 0, 0
	recs, more, err = scanRecordsUntil(rows, func(r Record) bool {
		size += r.textSize()
		if n >= limit || (n > 0 && size > maxBytes) {
			return false
		}
		n++
		return true
	})
	if err != nil {
		return nil, false, 0, err
	}
	err = tx.QueryRowContext(ctx, `SELECT seq FROM pruned WHERE owner_kind = ? AND owner_id = ?`, o.Kind, o.ID).Scan(&pruned)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, 0, err
	}
	return recs, more, pruned, nil
}

// textSize is about how many bytes r takes once sent: its text plus room
// for its numbers and field tags.
func (r Record) textSize() int {
	return 128 + len(r.OwnerKind) + len(r.OwnerID) + len(r.Kind) + len(r.Harness) + len(r.Event) +
		len(r.Tool) + len(r.ClaimedSession) + len(r.ToolUseID) + len(r.AgentID) + len(r.AgentType) +
		len(r.Cwd) + len(r.OldCwd) + len(r.NewCwd) + len(r.Targets) + len(r.Command) + len(r.Decision) +
		len(r.Rule) + len(r.PrevHash) + len(r.Hash)
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	recs, _, err := scanRecordsUntil(rows, func(Record) bool { return true })
	return recs, err
}

// scanRecordsUntil reads rows into records while keep accepts each one;
// more says a row was left unread.
func scanRecordsUntil(rows *sql.Rows, keep func(Record) bool) (out []Record, more bool, err error) {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.OwnerKind, &r.OwnerID, &r.Seq, &r.TS, &r.HookAt, &r.Kind, &r.Harness,
			&r.Event, &r.Tool, &r.ClaimedSession, &r.ToolUseID, &r.AgentID, &r.AgentType, &r.Cwd,
			&r.OldCwd, &r.NewCwd, &r.Targets, &r.Command, &r.InputSize, &r.Decision, &r.Rule,
			&r.PolicyRev, &r.Dropped, &r.PrevHash, &r.Hash); err != nil {
			return nil, false, err
		}
		if !keep(r) {
			return out, true, rows.Err()
		}
		out = append(out, r)
	}
	return out, false, rows.Err()
}

// OwnerActivity sums one owner's tool events in a window, for the summary.
type OwnerActivity struct {
	Owner   Owner
	Calls   int64 // pre-tool-use events
	Refused int64 // of those, calls the guard refused
	// Moves counts cwd-changed events by the directory moved to.
	Moves map[string]int64
}

// ActivitySince sums every owner's tool events orchd took at or after
// since (unix nanoseconds), ordered by owner.
func (s *Store) ActivitySince(ctx context.Context, since int64) ([]OwnerActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_kind, owner_id, event, new_cwd,
		COUNT(*), COALESCE(SUM(decision = ?), 0)
		FROM events WHERE ts >= ? AND kind = ?
		GROUP BY owner_kind, owner_id, event, new_cwd
		ORDER BY owner_kind, owner_id`, DecisionRefused, since, KindEvent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OwnerActivity
	for rows.Next() {
		var o Owner
		var event, dir string
		var n, refused int64
		if err := rows.Scan(&o.Kind, &o.ID, &event, &dir, &n, &refused); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Owner != o {
			out = append(out, OwnerActivity{Owner: o, Moves: map[string]int64{}})
		}
		a := &out[len(out)-1]
		switch event {
		case EventPreToolUse:
			a.Calls += n
			a.Refused += refused
		case EventCwdChanged:
			a.Moves[dir] += n
		}
	}
	return out, rows.Err()
}
