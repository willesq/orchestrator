package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// AccountRouter sends GitHub calls about an owner through the gh login its
// space names (SP-074): a space's own gh profile, or the default login.
type AccountRouter interface {
	// Account returns the runner and identity for calls about owner. ok is
	// false when they run as the default login. An error refuses the call:
	// it is never retried as another login.
	Account(ctx context.Context, owner string) (gh Runner, id Identity, ok bool, err error)
	// ScopeOwners lists the accounts only a space's own gh profile reaches,
	// each with the kind of scope it gets, so they can be watched.
	ScopeOwners(ctx context.Context) []Scope
}

// account returns the runner and identity for calls about owner: the
// router's when it routes owner, else the default login's.
func (s *Service) account(ctx context.Context, owner string) (Runner, Identity, error) {
	if s.Accounts != nil {
		gh, id, ok, err := s.Accounts.Account(ctx, owner)
		if err != nil {
			return nil, Identity{}, err
		}
		if ok {
			return gh, id, nil
		}
	}
	id, err := s.Identity(ctx, false)
	return s.gh, id, err
}

// Member reports whether owner is the account, or one of the orgs, of the
// gh login that calls about owner run as.
func (s *Service) Member(ctx context.Context, owner string) (bool, error) {
	_, id, err := s.account(ctx, owner)
	if err != nil {
		return false, err
	}
	return id.Member(owner), nil
}

// syncProfileScopes adds a disabled scope for each account only a space's own
// gh profile reaches, keeping rows the user already has.
func (s *Service) syncProfileScopes(ctx context.Context) error {
	if s.Accounts == nil {
		return nil
	}
	for _, sc := range s.Accounts.ScopeOwners(ctx) {
		if !ValidLogin(sc.Owner) || sc.Kind != KindUser && sc.Kind != KindOrg {
			continue
		}
		if err := s.Store.add(ctx, sc.Owner, sc.Kind, false); err != nil {
			return err
		}
	}
	return nil
}

// ErrNoAccess marks a call a space's gh profile made about a repo or org its
// account cannot read. It is not retried as another login.
var ErrNoAccess = errors.New("this space's gh login cannot read it, and orch does not try another login")

// noAccessSigns are what gh prints when the account cannot see the target:
// GitHub answers 404 for private repos and orgs it hides, 403 for ones it
// shows but refuses.
var noAccessSigns = []string{
	"HTTP 404", "HTTP 403", "Could not resolve to a", "Not Found", "Resource not accessible",
}

// throttleSigns mark a 403 that is GitHub limiting the account, not refusing
// it the target.
var throttleSigns = []string{"rate limit", "abuse", "secondary"}

// MarkNoAccess wraps err with ErrNoAccess when gh said the account cannot
// read the target, keeping gh's own words. A rate limit is not marked.
func MarkNoAccess(err error) error {
	if err == nil || errors.Is(err, ErrNoAccess) {
		return err
	}
	low := strings.ToLower(err.Error())
	for _, sign := range throttleSigns {
		if strings.Contains(low, sign) {
			return err
		}
	}
	for _, sign := range noAccessSigns {
		if strings.Contains(err.Error(), sign) {
			return fmt.Errorf("%w (%v)", ErrNoAccess, err)
		}
	}
	return err
}

// ErrProfileDir marks a gh_config_dir orch will not run gh with.
var ErrProfileDir = errors.New("not usable as a gh profile")

// CheckProfileDir checks that dir can be a gh profile (SP-074): an absolute
// path to an existing directory under the user's home (after resolving
// symbolic links), owned by the user and closed to everyone else. gh keeps
// a login in it, so another user able to read or replace it could act as, or
// substitute, the account. It reads only the directory's metadata.
func CheckProfileDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q is not an absolute path", ErrProfileDir, dir)
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return fmt.Errorf("%w: the home directory is unknown", ErrProfileDir)
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%w: %q does not exist", ErrProfileDir, dir)
	}
	rel, err := filepath.Rel(home, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is not inside your home directory", ErrProfileDir, dir)
	}
	// Reason: another user who can write a folder above the profile could
	// rename it and put their own in its place.
	for p := filepath.Dir(real); p != home && len(p) > len(home); p = filepath.Dir(p) {
		if err := ownedClosed(p, 0o022); err != nil {
			return fmt.Errorf("%w: %q is inside %q, which %v", ErrProfileDir, dir, p, err)
		}
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %q cannot be read: %v", ErrProfileDir, dir, err)
	case !fi.IsDir():
		return fmt.Errorf("%w: %q is not a directory", ErrProfileDir, dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %q is owned by another user", ErrProfileDir, dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: other users can open %q; make it yours alone (chmod 700)", ErrProfileDir, dir)
	}
	return nil
}

// ownedClosed checks that folder p is the user's and has none of the
// permission bits in mask.
func ownedClosed(p string, mask os.FileMode) error {
	fi, err := os.Stat(p)
	switch {
	case err != nil:
		return fmt.Errorf("cannot be read: %v", err)
	case fi.Mode().Perm()&mask != 0:
		return errors.New("other users can change")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return errors.New("is owned by another user")
	}
	return nil
}

// ErrQuota refuses a docs sync or PR watch call of an isolated space's own gh
// account whose hourly GitHub API budget is spent (SP-074); the call waits
// for the next hour. Other accounts are not affected.
var ErrQuota = errors.New("this gh account's hourly GitHub API budget is spent; its docs sync and PR watching resume within the hour")
