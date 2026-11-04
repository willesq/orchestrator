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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// What an owner's repo selection means.
const (
	ReposAll     = "all"
	ReposInclude = "include"
	ReposExclude = "exclude"
)

// Repo statuses that are not the outcome of a sync.
const (
	// StatusPaused marks an owner turned off.
	StatusPaused = "paused"
	// StatusExcluded marks a repo no longer synced, whose indexed docs stay.
	StatusExcluded = "excluded"
)

// ErrNoOwner is returned for an owner that is not registered.
var ErrNoOwner = errors.New("docs owner is not registered")

const minRefreshSecs = 60

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

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
	// Enabled owners are synced; a paused one keeps its row and indexed docs.
	Enabled bool
	// RepoMode says how Repos selects: ReposAll (empty means the same),
	// ReposInclude or ReposExclude.
	RepoMode string
	Repos    []string
}

// Covers reports whether the owner's selection takes in a repo. GitHub names
// are case-insensitive, so the selection is too.
func (o Owner) Covers(repo string) bool {
	listed := slices.ContainsFunc(o.Repos, func(r string) bool { return strings.EqualFold(r, repo) })
	switch o.RepoMode {
	case ReposInclude:
		return listed
	case ReposExclude:
		return !listed
	}
	return true
}

// Choice is what the operator decides about an owner.
type Choice struct {
	Enabled  bool
	RepoMode string
	Repos    []string
	// RefreshSecs replaces the cadence when above zero.
	RefreshSecs int64
}

// Validate checks a choice before it is stored.
func (c Choice) Validate() error {
	switch c.RepoMode {
	case ReposAll:
		if len(c.Repos) > 0 {
			return fmt.Errorf("repo mode %q takes no repos", c.RepoMode)
		}
	case ReposInclude, ReposExclude:
		if len(c.Repos) == 0 {
			return fmt.Errorf("repo mode %q needs at least one repo", c.RepoMode)
		}
	default:
		return fmt.Errorf("repo mode must be %q, %q or %q, got %q", ReposAll, ReposInclude, ReposExclude, c.RepoMode)
	}
	for _, r := range c.Repos {
		if !repoNameRe.MatchString(r) {
			return fmt.Errorf("invalid repo name %q", r)
		}
	}
	// Each sync lists the owner's repos, which spends the GitHub rate limit.
	if c.RefreshSecs != 0 && c.RefreshSecs < minRefreshSecs {
		return fmt.Errorf("refresh must be at least %d seconds", minRefreshSecs)
	}
	return nil
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

// canonicalOwner returns the spelling an owner is registered under, or login
// when it is new. GitHub logins are case-insensitive, while purging and syncing
// go by the exact spelling, so two rows for one owner would act on each other's
// docs.
func (r *Registry) canonicalOwner(ctx context.Context, login string) (string, error) {
	var owner string
	err := r.db.QueryRowContext(ctx, `SELECT owner FROM docs_owners WHERE owner=? COLLATE NOCASE`, login).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return login, nil
	}
	return owner, err
}

// AddOwner upserts an owner (idempotent by login, whatever its case).
func (r *Registry) AddOwner(ctx context.Context, o Owner) error {
	canonical, err := r.canonicalOwner(ctx, o.Owner)
	if err != nil {
		return err
	}
	o.Owner = canonical
	if o.Status == "" {
		o.Status = "new"
	}
	_, err = r.db.ExecContext(ctx,
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
		`SELECT owner, refresh_secs, scope, tags, last_synced, status, error, enabled, repo_mode, repos FROM docs_owners ORDER BY owner`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanOwners(rows)
}

// DueOwners returns owners whose refresh interval has elapsed by `now`.
func (r *Registry) DueOwners(ctx context.Context, now int64) ([]Owner, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT owner, refresh_secs, scope, tags, last_synced, status, error, enabled, repo_mode, repos
		 FROM docs_owners WHERE enabled=1 AND (? - last_synced) >= refresh_secs ORDER BY owner`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanOwners(rows)
}

// SetChoice stores what the operator decided about an owner. Repos it stops
// covering are marked excluded, which clears any failure they showed, and keep
// their indexed docs and catalog entry. A repo it covers again forgets its HEAD,
// so the next sync walks it and its status reflects that sync; unchanged files
// are not embedded again. Widening or resuming makes the owner due.
func (r *Registry) SetChoice(ctx context.Context, owner string, c Choice) (Owner, error) {
	if err := c.Validate(); err != nil {
		return Owner{}, err
	}
	all, err := r.ListOwners(ctx)
	if err != nil {
		return Owner{}, err
	}
	i := slices.IndexFunc(all, func(o Owner) bool { return strings.EqualFold(o.Owner, owner) })
	if i < 0 {
		return Owner{}, fmt.Errorf("%w: %s", ErrNoOwner, owner)
	}
	prev := all[i]
	owner = prev.Owner
	next := prev
	next.Enabled, next.RepoMode, next.Repos = c.Enabled, c.RepoMode, c.Repos
	if c.RefreshSecs > 0 {
		next.RefreshSecs = c.RefreshSecs
	}
	repos, err := r.ListRepos(ctx, owner)
	if err != nil {
		return Owner{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Owner{}, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, rp := range repos {
		covered := next.Enabled && next.Covers(rp.Repo)
		switch {
		case !covered && rp.Status != StatusExcluded:
			_, err = tx.ExecContext(ctx, `UPDATE docs_repos SET status=?, error='' WHERE owner=? AND repo=?`,
				StatusExcluded, owner, rp.Repo)
		case covered && rp.Status == StatusExcluded:
			_, err = tx.ExecContext(ctx, `UPDATE docs_repos SET head_sha='', status='new', error='' WHERE owner=? AND repo=?`,
				owner, rp.Repo)
		}
		if err != nil {
			return Owner{}, err
		}
	}
	changed := prev.RepoMode != next.RepoMode || !slices.Equal(prev.Repos, next.Repos)
	switch {
	case !next.Enabled:
		next.Status, next.Error = StatusPaused, ""
	case !prev.Enabled:
		next.Status, next.LastSynced = "new", 0
	case changed:
		next.LastSynced = 0
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE docs_owners SET enabled=?, repo_mode=?, repos=?, refresh_secs=?, last_synced=?, status=?, error=? WHERE owner=?`,
		next.Enabled, next.RepoMode, strings.Join(next.Repos, ","), next.RefreshSecs, next.LastSynced, next.Status, next.Error, owner); err != nil {
		return Owner{}, err
	}
	return next, tx.Commit()
}

// ForgetRepo drops a repo's sync state and the hashes of its docs, once its
// indexed docs are gone.
func (r *Registry) ForgetRepo(ctx context.Context, owner, repo string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos WHERE owner=? AND repo=?`, owner, repo); err != nil {
		return err
	}
	return r.deleteHashes(ctx, "repo://"+owner+"/"+repo+"/")
}

// RemoveOwner deletes an owner and its per-repo + per-file caches.
func (r *Registry) RemoveOwner(ctx context.Context, owner string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_owners WHERE owner=?`, owner); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos WHERE owner=?`, owner); err != nil {
		return err
	}
	return r.deleteHashes(ctx, "repo://"+owner+"/")
}

// deleteHashes forgets the hashes under a URI prefix ending in "/". A range,
// not LIKE, which ignores case and reads _ and % in a name as wildcards.
func (r *Registry) deleteHashes(ctx context.Context, prefix string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM doc_hashes WHERE uri >= ? AND uri < ?`,
		prefix, strings.TrimSuffix(prefix, "/")+"0")
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
	// Opted-out repos are not synced, so nothing would restore what is cleared:
	// their hashes are how a later purge finds the docs they still hold.
	where, args := `status != ?`, []any{StatusExcluded}
	if owner != "" {
		where, args = where+` AND owner=?`, append(args, owner)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM docs_repos WHERE `+where, args...); err != nil {
		return err
	}
	kept := map[string]bool{}
	rows, err := r.db.QueryContext(ctx, `SELECT owner, repo FROM docs_repos WHERE status = ?`, StatusExcluded)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o, repo string
		if err := rows.Scan(&o, &repo); err != nil {
			return err
		}
		kept["repo://"+o+"/"+repo+"/"] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	prefix := "repo://"
	if owner != "" {
		prefix += owner + "/"
	}
	uris, err := r.HashURIs(ctx, prefix)
	if err != nil {
		return err
	}
	for _, uri := range uris {
		if keptURI(kept, uri) {
			continue
		}
		if err := r.DeleteHash(ctx, uri); err != nil {
			return err
		}
	}
	return nil
}

// keptURI reports whether a doc URI (repo://owner/repo/path) is in a kept repo.
func keptURI(kept map[string]bool, uri string) bool {
	rest, ok := strings.CutPrefix(uri, "repo://")
	if !ok {
		return false
	}
	parts := strings.SplitN(rest, "/", 3)
	return len(parts) == 3 && kept["repo://"+parts[0]+"/"+parts[1]+"/"]
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

// ForgetRepoSHA clears a repo's recorded HEAD, so the next sync walks it
// again even though HEAD has not moved.
func (r *Registry) ForgetRepoSHA(ctx context.Context, owner, repo string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE docs_repos SET head_sha='' WHERE owner=? AND repo=?`, owner, repo)
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
		var tags, repos string
		if err := rows.Scan(&o.Owner, &o.RefreshSecs, &o.Scope, &tags, &o.LastSynced, &o.Status, &o.Error,
			&o.Enabled, &o.RepoMode, &repos); err != nil {
			return nil, err
		}
		if tags != "" {
			o.Tags = strings.Split(tags, ",")
		}
		if repos != "" {
			o.Repos = strings.Split(repos, ",")
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
