// Package sources is the unified registry + freshness scheduler for everything
// ingestable on a cadence: web URLs, repos, and owners (a user/org whose repos'
// markdown is ingested). The scheduler walks the registry, refreshes due sources,
// and on absence flags status='missing' while retaining the existing chunks.
package sources

import (
	"context"
	"database/sql"
	"strings"
)

// Kinds of source.
const (
	KindWeb   = "web"
	KindRepo  = "repo"
	KindOwner = "owner"
)

// Source is one registered, cadence-refreshed source.
type Source struct {
	ID          string
	Kind        string
	URI         string
	Scope       string
	Tags        []string
	Owner       string
	RefreshSecs int64
	LastFetched int64 // unix seconds
	Status      string
}

// Registry is the SQLite-backed store of sources (orch.db).
type Registry struct {
	db *sql.DB
}

// NewRegistry wraps an open orch.db handle.
func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

// ID derives the deterministic primary key for a source.
func ID(kind, uri string) string { return kind + "|" + uri }

// Add upserts a source (idempotent by kind+uri).
func (r *Registry) Add(ctx context.Context, s Source) error {
	if s.ID == "" {
		s.ID = ID(s.Kind, s.URI)
	}
	if s.Status == "" {
		s.Status = "new"
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sources (id, kind, uri, scope, tags, owner, refresh_secs, last_fetched, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   scope=excluded.scope, tags=excluded.tags, owner=excluded.owner,
		   refresh_secs=excluded.refresh_secs`,
		s.ID, s.Kind, s.URI, s.Scope, strings.Join(s.Tags, ","), s.Owner, s.RefreshSecs, s.LastFetched, s.Status)
	return err
}

// List returns all sources.
func (r *Registry) List(ctx context.Context) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, kind, uri, scope, tags, owner, refresh_secs, last_fetched, status FROM sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scan(rows)
}

// Due returns sources whose refresh interval has elapsed by `now`.
func (r *Registry) Due(ctx context.Context, now int64) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, kind, uri, scope, tags, owner, refresh_secs, last_fetched, status
		 FROM sources WHERE (? - last_fetched) >= refresh_secs ORDER BY id`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scan(rows)
}

// Remove deletes a source by id.
func (r *Registry) Remove(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	return err
}

// MarkFetched records a refresh outcome (status + fetch time).
func (r *Registry) MarkFetched(ctx context.Context, id string, now int64, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sources SET last_fetched=?, status=? WHERE id=?`, now, status, id)
	return err
}

// MarkDue resets a source (or all, if id=="") to be refreshed on the next scan.
func (r *Registry) MarkDue(ctx context.Context, id string) error {
	if id == "" {
		_, err := r.db.ExecContext(ctx, `UPDATE sources SET last_fetched=0`)
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE sources SET last_fetched=0 WHERE id=?`, id)
	return err
}

func scan(rows *sql.Rows) ([]Source, error) {
	var out []Source
	for rows.Next() {
		var s Source
		var tags string
		if err := rows.Scan(&s.ID, &s.Kind, &s.URI, &s.Scope, &tags, &s.Owner,
			&s.RefreshSecs, &s.LastFetched, &s.Status); err != nil {
			return nil, err
		}
		if tags != "" {
			s.Tags = strings.Split(tags, ",")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
