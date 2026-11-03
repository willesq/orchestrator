// Package github holds what the user monitors on GitHub: their own account and
// the orgs they opted in to (scopes), and who they are there (login, orgs,
// teams). orchd is the only caller of gh; clients reach this through RPCs.
package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Scope kinds.
const (
	KindUser = "user"
	KindOrg  = "org"
)

// Items modes: what a scope's PRs and issues include.
const (
	ItemsInvolving = "involving" // authored, assigned, mentioned, review requested of the user or their team
	ItemsAll       = "all"       // every open PR and issue
)

// Repo modes: which of a scope's repos count.
const (
	ReposAll     = "all"     // every repo the user can see
	ReposInclude = "include" // only Repos
	ReposExclude = "exclude" // all but Repos
)

// Scope is one GitHub account the watcher may monitor.
type Scope struct {
	Owner    string
	Kind     string
	Enabled  bool
	Items    string
	RepoMode string
	Repos    []string
	// EverythingRepos are watched for every open item while Items is
	// ItemsInvolving. They are kept while Items is ItemsAll, so switching back
	// restores them.
	EverythingRepos []string
	UpdatedAt       int64 // unix seconds
}

var (
	// GitHub's rules: alphanumerics and single hyphens, at most 39 characters.
	loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ErrNotFound is returned for an owner with no scope row.
var ErrNotFound = errors.New("no such GitHub scope")

// ValidLogin reports whether s is a well-formed GitHub login. The regexp alone
// allows more than 39 characters (each "-x" step takes two), hence the length.
func ValidLogin(s string) bool { return len(s) <= 39 && loginRe.MatchString(s) }

// ValidRepo reports whether s is a well-formed repo name, without its owner.
func ValidRepo(s string) bool { return repoRe.MatchString(s) && s != "." && s != ".." }

// Validate checks a scope's fields before it is stored.
func (s Scope) Validate() error {
	if !ValidLogin(s.Owner) {
		return fmt.Errorf("invalid GitHub login %q", s.Owner)
	}
	if s.Kind != KindUser && s.Kind != KindOrg {
		return fmt.Errorf("kind must be %q or %q, got %q", KindUser, KindOrg, s.Kind)
	}
	if s.Items != ItemsInvolving && s.Items != ItemsAll {
		return fmt.Errorf("items must be %q or %q, got %q", ItemsInvolving, ItemsAll, s.Items)
	}
	switch s.RepoMode {
	case ReposAll:
		if len(s.Repos) > 0 {
			return fmt.Errorf("repo mode %q takes no repos", ReposAll)
		}
	case ReposInclude, ReposExclude:
		if len(s.Repos) == 0 {
			return fmt.Errorf("repo mode %q needs at least one repo", s.RepoMode)
		}
	default:
		return fmt.Errorf("repo mode must be %q, %q or %q, got %q", ReposAll, ReposInclude, ReposExclude, s.RepoMode)
	}
	for _, r := range slices.Concat(s.Repos, s.EverythingRepos) {
		if !ValidRepo(r) {
			return fmt.Errorf("invalid repo name %q", r)
		}
	}
	return nil
}

// Watches reports whether the scope covers repo (a name within Owner).
func (s Scope) Watches(repo string) bool {
	if !s.Enabled {
		return false
	}
	// Naming a repo for everything is explicit, so it outranks the repo picks.
	if containsFold(s.WatchedForEverything(), repo) {
		return true
	}
	switch s.RepoMode {
	case ReposInclude:
		return containsFold(s.Repos, repo)
	case ReposExclude:
		return !containsFold(s.Repos, repo)
	default:
		return true
	}
}

// WatchedForEverything returns the repos that add searches of their own: none
// when the whole scope is already watched for everything.
func (s Scope) WatchedForEverything() []string {
	if s.Items != ItemsInvolving {
		return nil
	}
	return s.EverythingRepos
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// Store is the SQLite-backed set of scopes.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore wraps an open orch.db handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

// List returns every scope: the user's own account first, then orgs by name.
func (st *Store) List(ctx context.Context) ([]Scope, error) {
	rows, err := st.db.QueryContext(ctx,
		`SELECT owner, kind, enabled, items, repo_mode, repos, everything_repos, updated_at FROM github_scopes
		 ORDER BY CASE kind WHEN 'user' THEN 0 ELSE 1 END, lower(owner)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Scope
	for rows.Next() {
		var s Scope
		var enabled int
		var repos, everything string
		if err := rows.Scan(&s.Owner, &s.Kind, &enabled, &s.Items, &s.RepoMode, &repos, &everything, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled != 0
		s.Repos = splitRepos(repos)
		s.EverythingRepos = splitRepos(everything)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns one scope by login (case-insensitive), or ErrNotFound.
func (st *Store) Get(ctx context.Context, owner string) (Scope, error) {
	all, err := st.List(ctx)
	if err != nil {
		return Scope{}, err
	}
	for _, s := range all {
		if strings.EqualFold(s.Owner, owner) {
			return s, nil
		}
	}
	return Scope{}, ErrNotFound
}

// Set stores a scope after validating it, replacing any existing row.
func (st *Store) Set(ctx context.Context, s Scope) (Scope, error) {
	s.Repos = normaliseRepos(s.Repos)
	s.EverythingRepos = normaliseRepos(s.EverythingRepos)
	if err := s.Validate(); err != nil {
		return Scope{}, err
	}
	s.UpdatedAt = st.now().Unix()
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO github_scopes (owner, kind, enabled, items, repo_mode, repos, everything_repos, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner) DO UPDATE SET kind=excluded.kind, enabled=excluded.enabled,
		   items=excluded.items, repo_mode=excluded.repo_mode, repos=excluded.repos,
		   everything_repos=excluded.everything_repos, updated_at=excluded.updated_at`,
		s.Owner, s.Kind, boolInt(s.Enabled), s.Items, s.RepoMode, strings.Join(s.Repos, ","),
		strings.Join(s.EverythingRepos, ","), s.UpdatedAt)
	if err != nil {
		return Scope{}, err
	}
	return s, nil
}

// Sync adds a scope for each account in id that has none yet: the user's own
// account enabled, orgs disabled. Existing rows are left alone, so choices the
// user made survive rediscovery.
func (st *Store) Sync(ctx context.Context, id Identity) error {
	add := func(owner, kind string, enabled bool) error {
		_, err := st.db.ExecContext(ctx,
			`INSERT INTO github_scopes (owner, kind, enabled, updated_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(owner) DO NOTHING`, owner, kind, boolInt(enabled), st.now().Unix())
		return err
	}
	if ValidLogin(id.Login) {
		if err := add(id.Login, KindUser, true); err != nil {
			return err
		}
	}
	for _, org := range id.Orgs {
		if !ValidLogin(org) {
			continue
		}
		if err := add(org, KindOrg, false); err != nil {
			return err
		}
	}
	return nil
}

func normaliseRepos(repos []string) []string {
	var out []string
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r != "" && !containsFold(out, r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out
}

func splitRepos(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
