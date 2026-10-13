package routing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Store reads and writes the routing table and its history.
type Store struct{ db *sql.DB }

// NewStore wraps an open orch.db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// historyKeep is how many replaced tables are kept for rollback.
const historyKeep = 50

// ErrNoHistory is returned by Rollback when there is nothing to roll back to.
var ErrNoHistory = errors.New("no earlier routing table to roll back to")

const routeCols = `task_kind, rank, provider, model, effort, rationale, evidence_n, decided_at, pinned`

// Table returns the current routing table, by kind then rank.
func (s *Store) Table(ctx context.Context) ([]Route, error) {
	return queryRoutes(ctx, s.db, `SELECT `+routeCols+` FROM routing ORDER BY task_kind, rank`)
}

// Kind returns one kind's routes, by rank.
func (s *Store) Kind(ctx context.Context, kind string) ([]Route, error) {
	return queryRoutes(ctx, s.db, `SELECT `+routeCols+` FROM routing WHERE task_kind = ? ORDER BY rank`, kind)
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func queryRoutes(ctx context.Context, q querier, query string, args ...any) ([]Route, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.Kind, &r.Rank, &r.Provider, &r.Model, &r.Effort, &r.Rationale,
			&r.EvidenceN, &r.DecidedAt, &r.Pinned); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Apply replaces the table with proposed, keeping every kind that has a pinned
// route as it is. The replaced table is kept for Rollback. It reports how many
// kinds were left alone because they are pinned.
func (s *Store) Apply(ctx context.Context, proposed []Route, now int64) (pinnedKinds int, err error) {
	err = s.change(ctx, now, func(tx *sql.Tx) error {
		pinned := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT task_kind FROM routing WHERE pinned = 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				_ = rows.Close()
				return err
			}
			pinned[k] = true
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		pinnedKinds = len(pinned)
		if _, err := tx.ExecContext(ctx, `DELETE FROM routing WHERE task_kind NOT IN (SELECT task_kind FROM routing WHERE pinned = 1)`); err != nil {
			return err
		}
		for _, r := range proposed {
			if pinned[r.Kind] {
				continue
			}
			r.Pinned = false
			if err := insertRoute(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	return pinnedKinds, err
}

// Set pins one route as the only route for its kind.
func (s *Store) Set(ctx context.Context, r Route, now int64) error {
	if !ValidKind(r.Kind) {
		return fmt.Errorf("unknown task kind %q", r.Kind)
	}
	r.Rank, r.Pinned, r.DecidedAt = 1, true, now
	if r.Rationale == "" {
		r.Rationale = "set by hand"
	}
	return s.change(ctx, now, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM routing WHERE task_kind = ?`, r.Kind); err != nil {
			return err
		}
		return insertRoute(ctx, tx, r)
	})
}

// Clear removes a kind's routes, pinned or not, so the next apply fills it.
func (s *Store) Clear(ctx context.Context, kind string, now int64) error {
	if !ValidKind(kind) {
		return fmt.Errorf("unknown task kind %q", kind)
	}
	return s.change(ctx, now, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM routing WHERE task_kind = ?`, kind)
		return err
	})
}

// Rollback restores the table as it was before the most recent change.
func (s *Store) Rollback(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var gen sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(generation) FROM routing_history`).Scan(&gen); err != nil {
		return err
	}
	if !gen.Valid {
		return ErrNoHistory
	}
	prev, err := queryRoutes(ctx, tx, `SELECT `+routeCols+` FROM routing_history WHERE generation = ? AND rank > 0`, gen.Int64)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM routing`); err != nil {
		return err
	}
	for _, r := range prev {
		if err := insertRoute(ctx, tx, r); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM routing_history WHERE generation = ?`, gen.Int64); err != nil {
		return err
	}
	return tx.Commit()
}

// change snapshots the current table into history, then applies fn, in one
// transaction. An empty table is recorded as a marker row (rank 0), so rolling
// back to "no routes" works too.
func (s *Store) change(ctx context.Context, now int64, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var gen int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM routing_history`).Scan(&gen); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO routing_history (generation, `+routeCols+`, replaced_at)
		SELECT ?, `+routeCols+`, ? FROM routing`, gen, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO routing_history (generation, task_kind, rank, provider, model, decided_at, replaced_at)
			VALUES (?, '', 0, '', '', 0, ?)`, gen, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM routing_history WHERE generation <= ?`, gen-historyKeep); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func insertRoute(ctx context.Context, tx *sql.Tx, r Route) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO routing (`+routeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.Rank, r.Provider, r.Model, r.Effort, r.Rationale, r.EvidenceN, r.DecidedAt, r.Pinned)
	return err
}
