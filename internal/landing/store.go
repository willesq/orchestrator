package landing

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Record is what the user decided about a task's proposal.
type Record struct {
	TaskID, URL, Branch, Path, SHA, PriorHead, RevertSHA, State string
	UpdatedAt                                                   int64
}

// Store keeps decisions in orch.db's pr_landings table.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore builds a store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

const recordColumns = `task_id, url, branch, path, sha, prior_head, revert_sha, state, updated_at`

func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var r Record
	err := row.Scan(&r.TaskID, &r.URL, &r.Branch, &r.Path, &r.SHA, &r.PriorHead, &r.RevertSHA, &r.State, &r.UpdatedAt)
	return r, err
}

// Get returns the task's decision, if the user made one.
func (s *Store) Get(ctx context.Context, taskID string) (Record, bool, error) {
	r, err := scanRecord(s.db.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM pr_landings WHERE task_id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

// List returns every decision, newest first.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordColumns+` FROM pr_landings ORDER BY updated_at DESC, task_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Put writes the task's decision over any earlier one.
func (s *Store) Put(ctx context.Context, r Record) error {
	now := s.now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pr_landings (task_id, url, branch, path, sha, prior_head, revert_sha, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			url = excluded.url, branch = excluded.branch, path = excluded.path, sha = excluded.sha,
			prior_head = excluded.prior_head, revert_sha = excluded.revert_sha, state = excluded.state,
			updated_at = excluded.updated_at`,
		r.TaskID, r.URL, r.Branch, r.Path, r.SHA, r.PriorHead, r.RevertSHA, r.State, now, now)
	return err
}

// Delete forgets the task's decision.
func (s *Store) Delete(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pr_landings WHERE task_id = ?`, taskID)
	return err
}
