package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Row is one stored item: its desired state and what orch last saw.
type Row struct {
	Provider string
	Kind     string
	Identity string
	Enabled  bool
	Origin   Origin
	Payload  json.RawMessage
	// Present is whether the item was in its file at the last look.
	Present bool
	// RemovedAt is when it vanished from its file outside orch; zero if not.
	RemovedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Status derives the client-facing state.
func (r Row) Status() Status {
	switch {
	case !r.RemovedAt.IsZero():
		return StatusRemoved
	case !r.Enabled:
		return StatusDisabled
	case !r.Present:
		return StatusMissing
	default:
		return StatusActive
	}
}

// Store reads and writes settings_items in orch.db.
type Store struct {
	db  dbtx
	now func() time.Time
}

// dbtx is what Store runs queries on: the database, or a transaction.
type dbtx interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// inTx runs fn on a Store bound to one transaction, committed when fn
// succeeds and rolled back otherwise.
func (s *Store) inTx(ctx context.Context, fn func(*Store) error) error {
	db, ok := s.db.(*sql.DB)
	if !ok {
		return fn(s)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Store{db: tx, now: s.now}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// NewStore wraps an open, migrated orch.db.
func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

const rowColumns = `provider, kind, identity, enabled, origin, payload, present, removed_at, created_at, updated_at`

// List returns a provider's rows (every provider when empty), optionally of
// one kind, in a stable order.
func (s *Store) List(ctx context.Context, provider, kind string) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM settings_items
		WHERE (?1 = '' OR provider = ?1) AND (?2 = '' OR kind = ?2)
		ORDER BY provider, kind, identity`, provider, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one row.
func (s *Store) Get(ctx context.Context, provider, kind, identity string) (Row, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM settings_items
		WHERE provider = ? AND kind = ? AND identity = ?`, provider, kind, identity))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// Put inserts or replaces a row, keeping its created_at and stamping
// updated_at.
func (s *Store) Put(ctx context.Context, r Row) error {
	now := s.now().Unix()
	created := now
	if !r.CreatedAt.IsZero() {
		created = r.CreatedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings_items (`+rowColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, kind, identity) DO UPDATE SET
			enabled = excluded.enabled, origin = excluded.origin, payload = excluded.payload,
			present = excluded.present, removed_at = excluded.removed_at, updated_at = excluded.updated_at`,
		r.Provider, r.Kind, r.Identity, boolInt(r.Enabled), string(r.Origin), string(r.Payload),
		boolInt(r.Present), unix(r.RemovedAt), created, now)
	return err
}

// Delete removes a row.
func (s *Store) Delete(ctx context.Context, provider, kind, identity string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings_items WHERE provider = ? AND kind = ? AND identity = ?`,
		provider, kind, identity)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner) (Row, error) {
	var r Row
	var enabled, present int
	var origin, payload string
	var removed, created, updated int64
	if err := sc.Scan(&r.Provider, &r.Kind, &r.Identity, &enabled, &origin, &payload, &present,
		&removed, &created, &updated); err != nil {
		return Row{}, err
	}
	r.Enabled, r.Present = enabled != 0, present != 0
	r.Origin, r.Payload = Origin(origin), json.RawMessage(payload)
	r.RemovedAt, r.CreatedAt, r.UpdatedAt = fromUnix(removed), fromUnix(created), fromUnix(updated)
	return r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(s int64) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}
