package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Team is one GitHub team the user is on.
type Team struct {
	Org  string
	Slug string
	Name string
}

// Identity is who the user is on GitHub, as gh sees them.
type Identity struct {
	Login string
	Orgs  []string
	Teams []Team
}

// Repo is one repository the user can see under an owner.
type Repo struct {
	Name     string
	Private  bool
	Archived bool
}

// Runner runs gh with args and returns stdout; injectable for tests.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// GH runs the real gh CLI.
func GH(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), Printable(string(exitErr.Stderr), 512))
		}
		return nil, err
	}
	return out, nil
}

// Printable trims s, drops control characters (gh errors and team names go to
// terminals and the app), and caps it at max bytes.
func Printable(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s))
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

// identityTTL bounds how often discovery calls GitHub: memberships change
// rarely, and the app asks every time its GitHub tab opens.
const identityTTL = 10 * time.Minute

// minRefresh bounds forced refreshes, so a client asking repeatedly cannot
// spend the user's GitHub rate limit.
const minRefresh = 30 * time.Second

// ErrNotMember is returned for an owner that is neither the user nor one of
// their orgs.
var ErrNotMember = errors.New("not your account or an org you belong to")

// Service is the scope store plus identity discovery, with discovery cached.
type Service struct {
	Store *Store
	gh    Runner
	now   func() time.Time

	mu       sync.Mutex
	identity *Identity
	fetched  time.Time
}

// NewService builds a service over store using gh to reach GitHub.
func NewService(store *Store, gh Runner) *Service {
	return &Service{Store: store, gh: gh, now: time.Now}
}

// Identity returns the user's login, orgs and teams, from cache unless it is
// older than identityTTL or refresh is set.
func (s *Service) Identity(ctx context.Context, refresh bool) (Identity, error) {
	s.mu.Lock()
	if s.identity != nil {
		age := s.now().Sub(s.fetched)
		if age < minRefresh || !refresh && age < identityTTL {
			id := s.identity.clone()
			s.mu.Unlock()
			return id, nil
		}
	}
	s.mu.Unlock()

	id, err := discover(ctx, s.gh)
	if err != nil {
		return Identity{}, err
	}
	s.mu.Lock()
	cached := id.clone()
	s.identity, s.fetched = &cached, s.now()
	s.mu.Unlock()
	return id, nil
}

func (id Identity) clone() Identity {
	id.Orgs = slices.Clone(id.Orgs)
	id.Teams = slices.Clone(id.Teams)
	return id
}

// Member reports whether owner is the user or one of their orgs.
func (id Identity) Member(owner string) bool {
	return strings.EqualFold(owner, id.Login) || slices.ContainsFunc(id.Orgs, func(o string) bool { return strings.EqualFold(o, owner) })
}

// Scopes discovers the identity, adds scopes for accounts that have none, and
// returns them all. When GitHub cannot be reached the stored scopes are still
// returned, with that failure in ghErr; err is a storage failure.
func (s *Service) Scopes(ctx context.Context, refresh bool) (scopes []Scope, ghErr, err error) {
	id, ghErr := s.Identity(ctx, refresh)
	if ghErr == nil {
		if err := s.Store.Sync(ctx, id); err != nil {
			return nil, nil, err
		}
	}
	scopes, err = s.Store.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	return scopes, ghErr, nil
}

// Repos lists the repos the user can see under owner, for the repo picker.
func (s *Service) Repos(ctx context.Context, owner string) ([]Repo, error) {
	if !ValidLogin(owner) {
		return nil, fmt.Errorf("invalid GitHub login %q", owner)
	}
	id, err := s.Identity(ctx, false)
	if err != nil {
		return nil, err
	}
	if !id.Member(owner) {
		return nil, fmt.Errorf("%s: %w", owner, ErrNotMember)
	}
	// The user's own account goes through /user/repos, which includes their
	// private repos; any other owner is listed as an org.
	path := "orgs/" + owner + "/repos?per_page=100"
	if strings.EqualFold(owner, id.Login) {
		path = "user/repos?per_page=100&affiliation=owner"
	}
	out, err := s.gh(ctx, "api", "--paginate", path, "--jq", ".[] | {name, private, archived}")
	if err != nil {
		return nil, err
	}
	var repos []Repo
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var r struct {
			Name     string `json:"name"`
			Private  bool   `json:"private"`
			Archived bool   `json:"archived"`
		}
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("parse repos: %w", err)
		}
		repos = append(repos, Repo{Name: r.Name, Private: r.Private, Archived: r.Archived})
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
	return repos, nil
}

// Account is a GitHub account's login, as GitHub spells it, and its node ID.
// The node ID stays with the account when it is renamed and is never given to
// another account, so it tells a re-registered login from the original.
type Account struct {
	Login  string
	NodeID string
}

// maxNodeID bounds a node ID read from gh; GitHub's are under 40 bytes.
const maxNodeID = 128

// LookupAccount resolves login, an org or a user, to its Account. It fails
// when GitHub returns a different login, so a caller never binds an account
// it did not ask for.
func LookupAccount(ctx context.Context, gh Runner, login string) (Account, error) {
	if !ValidLogin(login) {
		return Account{}, fmt.Errorf("invalid GitHub login %q", login)
	}
	// users/<login> answers for orgs too, so a space may bind the user's own
	// account as well as their orgs.
	out, err := gh(ctx, "api", "users/"+login, "--jq", "{login, node_id}")
	if err != nil {
		return Account{}, err
	}
	var a struct {
		Login  string `json:"login"`
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(out, &a); err != nil {
		return Account{}, fmt.Errorf("parse GitHub account %s: %w", login, err)
	}
	switch {
	case !strings.EqualFold(a.Login, login):
		return Account{}, fmt.Errorf("GitHub returned account %q for %q", Printable(a.Login, 64), login)
	case a.NodeID == "" || len(a.NodeID) > maxNodeID || Printable(a.NodeID, maxNodeID) != a.NodeID:
		return Account{}, fmt.Errorf("GitHub returned no usable node ID for %q", login)
	}
	return Account{Login: a.Login, NodeID: a.NodeID}, nil
}

// RepoInfo is a repo as GitHub reports it now: after a transfer or rename,
// Owner and Name are the current ones.
type RepoInfo struct {
	Owner   string
	Name    string
	Private bool
}

// LookupRepo reads owner/name's current owner and visibility. GitHub
// redirects a transferred repo's old name, so Owner can differ from owner.
func LookupRepo(ctx context.Context, gh Runner, owner, name string) (RepoInfo, error) {
	if !ValidLogin(owner) || !ValidRepo(name) {
		return RepoInfo{}, fmt.Errorf("invalid GitHub repo %q", owner+"/"+name)
	}
	out, err := gh(ctx, "api", "repos/"+owner+"/"+name, "--jq", "{owner: .owner.login, name, private}")
	if err != nil {
		return RepoInfo{}, err
	}
	var r struct {
		Owner   string `json:"owner"`
		Name    string `json:"name"`
		Private *bool  `json:"private"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return RepoInfo{}, fmt.Errorf("parse GitHub repo %s/%s: %w", owner, name, err)
	}
	if !ValidLogin(r.Owner) || !ValidRepo(r.Name) || r.Private == nil {
		return RepoInfo{}, fmt.Errorf("GitHub returned no usable repo for %s/%s", owner, name)
	}
	return RepoInfo{Owner: r.Owner, Name: r.Name, Private: *r.Private}, nil
}

func discover(ctx context.Context, gh Runner) (Identity, error) {
	var id Identity
	out, err := gh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return id, err
	}
	id.Login = strings.TrimSpace(string(out))
	if !ValidLogin(id.Login) {
		return Identity{}, fmt.Errorf("gh returned no usable login (%q)", id.Login)
	}
	if out, err = gh(ctx, "api", "--paginate", "user/orgs", "--jq", ".[].login"); err != nil {
		return Identity{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if org := strings.TrimSpace(line); ValidLogin(org) {
			id.Orgs = append(id.Orgs, org)
		}
	}
	sort.Slice(id.Orgs, func(i, j int) bool { return strings.ToLower(id.Orgs[i]) < strings.ToLower(id.Orgs[j]) })
	if out, err = gh(ctx, "api", "--paginate", "user/teams", "--jq", ".[] | {org: .organization.login, slug, name}"); err != nil {
		return Identity{}, err
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var t struct {
			Org  string `json:"org"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if err := dec.Decode(&t); err != nil {
			return Identity{}, fmt.Errorf("parse teams: %w", err)
		}
		id.Teams = append(id.Teams, Team{Org: t.Org, Slug: t.Slug, Name: t.Name})
	}
	sort.Slice(id.Teams, func(i, j int) bool {
		a, b := id.Teams[i], id.Teams[j]
		if !strings.EqualFold(a.Org, b.Org) {
			return strings.ToLower(a.Org) < strings.ToLower(b.Org)
		}
		return a.Slug < b.Slug
	})
	return id, nil
}
