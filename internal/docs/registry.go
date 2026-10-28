// Package docs is the GitHub code-docs subsystem: it tracks GitHub owners
// (users/orgs), clones their repos into a managed cache, and ingests the repos'
// markdown into RAG on a cadence. It is deliberately separate from the generic
// `sources` registry — this one is GitHub-specific and code-repo-oriented, with
// repo-level (HEAD SHA) and file-level (content hash) skipping to minimize both
// GitHub API calls and redundant embedding work.
//
// Sleep-safe like `sources`: due-ness is computed from persisted last_synced
// timestamps, so an owner missed while the laptop slept is synced on the next scan.
package docs

import (
	"context"
	"database/sql"
	"strings"
)

// Owner is a registered GitHub owner (user or org) whose repos' docs are ingested.
type Owner struct {
	Owner       string
	RefreshSecs int64
	Scope       string
	Tags        []string
	LastSynced  int64 // unix seconds
	Status      string
	// Error says why the last sync failed or stopped early; empty when it
	// completed.
	Error string
}

// RepoState is the per-repo sync bookkeeping used to skip unchanged repos.
type RepoState struct {
	Owner      string
	Repo       string
	HeadSHA    string
	LastSynced int64
	Files      int
	Status     string
	// Error says why the repo's last sync failed; empty when it succeeded.
	Error string
}

// Registry is the SQLite-backed store of docs owners + per-repo/file sync state.
type Registry struct{ db *sql.DB }

// NewRegistry wraps an open orch.db handle.
func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

// AddOwner upserts an owner (idempotent by login).
func (r *Registry) AddOwner(ctx context.Context, o Owner) error {
	if o.Status == "" {
		o.Status = "new"
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO docs_owners (owner, refresh_secs, scope, tags, last_synced, status)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner) DO UPDATE SET
		   refresh_secs=excluded.refresh_secs, scope=excluded.scope, tags=excluded.tags`,
		o.Owner, o.RefreshSecs, o.Scope, strings.Join(o.Tags, ","), o.LastSynced, o.Status)
	return err
}

// ListOwners returns all registered owners.
func (r *Registry) ListOwners(ctx context.Context) ([]Owner, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT owner, refresh_secs, scope, tags, last_synced, status, error FROM docs_owners ORDER BY owner`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanOwners(rows)
}

// DueOwners returns owners whose refresh interval has elapsed by `now`.
func (r *Registry) DueOwners(ctx context.Context, now int64) ([]Owner, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT owner, refresh_secs, scope, tags, last_synced, status, error
		 FROM docs_owners WHERE (? - last_synced) >= refresh_secs ORDER BY owner`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanOwners(rows)
}

// RemoveOwner deletes an owner and its per-repo + per-file caches.
func (r *Registry) RemoveOwner(ctx context.Context, owner string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_owners WHERE owner=?`, owner); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos WHERE owner=?`, owner); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM doc_hashes WHERE uri LIKE ?`, "repo://"+owner+"/%")
	return err
}

// MarkSynced records an owner's sync outcome: when, its status, and why it
// failed or stopped early (empty when it completed).
func (r *Registry) MarkSynced(ctx context.Context, owner string, now int64, status, reason string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE docs_owners SET last_synced=?, status=?, error=? WHERE owner=?`, now, status, reason, owner)
	return err
}

// MarkStopped records why an owner's sync stopped before finishing, leaving
// its last_synced as it was, so the owner stays due and is synced again on the
// next scan.
func (r *Registry) MarkStopped(ctx context.Context, owner, reason string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE docs_owners SET status='error', error=? WHERE owner=?`, reason, owner)
	return err
}

// MarkDue resets an owner (or all, if owner=="") to be synced on the next scan.
func (r *Registry) MarkDue(ctx context.Context, owner string) error {
	if owner == "" {
		_, err := r.db.ExecContext(ctx, `UPDATE docs_owners SET last_synced=0`)
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE docs_owners SET last_synced=0 WHERE owner=?`, owner)
	return err
}

// ClearCaches drops the repo-SHA and file-hash caches for an owner (or all, if
// owner==""), forcing the next sync to re-clone and re-ingest everything.
func (r *Registry) ClearCaches(ctx context.Context, owner string) error {
	if owner == "" {
		if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos`); err != nil {
			return err
		}
		_, err := r.db.ExecContext(ctx, `DELETE FROM doc_hashes WHERE uri LIKE ?`, "repo://%")
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos WHERE owner=?`, owner); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM doc_hashes WHERE uri LIKE ?`, "repo://"+owner+"/%")
	return err
}

// RepoSHA returns the last-synced HEAD SHA for a repo ("" if never synced).
func (r *Registry) RepoSHA(ctx context.Context, owner, repo string) (string, error) {
	var sha string
	err := r.db.QueryRowContext(ctx,
		`SELECT head_sha FROM docs_repos WHERE owner=? AND repo=?`, owner, repo).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sha, err
}

// SetRepoState upserts per-repo sync bookkeeping.
func (r *Registry) SetRepoState(ctx context.Context, rs RepoState) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO docs_repos (owner, repo, head_sha, last_synced, files, status, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner, repo) DO UPDATE SET
		   head_sha=excluded.head_sha, last_synced=excluded.last_synced,
		   files=excluded.files, status=excluded.status, error=excluded.error`,
		rs.Owner, rs.Repo, rs.HeadSHA, rs.LastSynced, rs.Files, rs.Status, rs.Error)
	return err
}

// ListRepos returns the per-repo state for an owner.
func (r *Registry) ListRepos(ctx context.Context, owner string) ([]RepoState, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT owner, repo, head_sha, last_synced, files, status, error
		 FROM docs_repos WHERE owner=? ORDER BY repo`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RepoState
	for rows.Next() {
		var rs RepoState
		if err := rows.Scan(&rs.Owner, &rs.Repo, &rs.HeadSHA, &rs.LastSynced, &rs.Files, &rs.Status, &rs.Error); err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// Hash returns the stored content hash for a doc URI ("" if unknown).
func (r *Registry) Hash(ctx context.Context, uri string) (string, error) {
	var h string
	err := r.db.QueryRowContext(ctx, `SELECT hash FROM doc_hashes WHERE uri=?`, uri).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return h, err
}

// SetHash records a doc URI's content hash.
func (r *Registry) SetHash(ctx context.Context, uri, hash string, now int64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO doc_hashes (uri, hash, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(uri) DO UPDATE SET hash=excluded.hash, updated_at=excluded.updated_at`,
		uri, hash, now)
	return err
}

// HashURIs returns the doc URIs with a stored hash that start with prefix.
func (r *Registry) HashURIs(ctx context.Context, prefix string) ([]string, error) {
	// substr, not LIKE: '_' and '%' are common in repo and file names.
	rows, err := r.db.QueryContext(ctx,
		`SELECT uri FROM doc_hashes WHERE substr(uri, 1, ?) = ? ORDER BY uri`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteHash forgets a doc URI's stored hash.
func (r *Registry) DeleteHash(ctx context.Context, uri string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM doc_hashes WHERE uri=?`, uri)
	return err
}

func scanOwners(rows *sql.Rows) ([]Owner, error) {
	var out []Owner
	for rows.Next() {
		var o Owner
		var tags string
		if err := rows.Scan(&o.Owner, &o.RefreshSecs, &o.Scope, &tags, &o.LastSynced, &o.Status, &o.Error); err != nil {
			return nil, err
		}
		if tags != "" {
			o.Tags = strings.Split(tags, ",")
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Doc is one ingested markdown document, keyed by its repo:// URI.
type Doc struct {
	URI       string
	UpdatedAt int64 // unix seconds of the last content-changing ingest
}

// ListDocs returns every ingested document ordered by URI.
func (r *Registry) ListDocs(ctx context.Context) ([]Doc, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT uri, updated_at FROM doc_hashes WHERE uri LIKE 'repo://%' ORDER BY uri`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Doc
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.URI, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
