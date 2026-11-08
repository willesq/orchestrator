package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/spaces"
)

// ghProfiles picks the gh login each GitHub call orchd makes runs with
// (SP-063, D-18). A call about an org bound to a space with a gh profile runs
// as that profile's account; one about an isolated space's org without a
// logged-in profile is refused; anything else runs as the default login. A
// profile is checked only with 'gh auth status' and the account it reports:
// orch never reads inside a profile directory or asks gh for a token.
type ghProfiles struct {
	spaces *spaces.Engine
	// run runs gh with env, nil being the default login; github.GHEnv
	// outside tests.
	run func(ctx context.Context, env []string, args ...string) ([]byte, error)
	now func() time.Time

	mu      sync.Mutex
	checked map[string]profileCheck
}

// profileCheck is what 'gh auth status' and the account's orgs said about one
// profile directory, and when.
type profileCheck struct {
	login string
	orgs  []string
	err   error
	at    time.Time
}

// A profile's login rarely changes, so checks are cached; a failed one is
// retried sooner, so a login the user just made is picked up quickly.
const (
	profileCheckTTL     = 5 * time.Minute
	profileRetryTTL     = time.Minute
	profileCheckTimeout = 15 * time.Second
)

// Reason: these reach the docs list and orchd's log, which tasks can read,
// so they name no space; the operator sees which in 'orch status'.
var (
	errNoGHProfile    = errors.New("its space has no gh profile; the operator can see which in 'orch status'")
	errGHProfileLogin = errors.New("its space's gh profile is not logged in; the operator can see which in 'orch status'")
	errTwoOwners      = errors.New("the gh call names two GitHub accounts")
)

func newGHProfiles() *ghProfiles {
	return &ghProfiles{run: github.GHEnv, now: time.Now, checked: map[string]profileCheck{}}
}

// defaultProfile keys the default login's check, which has no directory.
const defaultProfile = ""

// Env returns the environment for gh calls about GitHub account owner: nil
// for the default login, or a profile's environment. An error refuses the
// call, including when the applied policy does not verify.
func (g *ghProfiles) Env(ctx context.Context, owner string) ([]string, error) {
	if g == nil || g.spaces == nil {
		return nil, nil
	}
	res, err := g.spaces.Current()
	if err != nil {
		return nil, err
	}
	prof, ok := res.GHProfileFor(owner)
	if !ok {
		return nil, nil
	}
	return g.profileEnv(ctx, prof)
}

// profileEnv returns the environment for gh calls as prof.
func (g *ghProfiles) profileEnv(ctx context.Context, prof spaces.GHProfile) ([]string, error) {
	env, _, err := g.profileAccount(ctx, prof)
	return env, err
}

// profileAccount returns the environment and login for gh calls as prof: nil
// and "" for the default login.
func (g *ghProfiles) profileAccount(ctx context.Context, prof spaces.GHProfile) ([]string, string, error) {
	switch {
	case prof.ConfigDir != "":
		c := g.check(ctx, prof.ConfigDir)
		if c.err != nil {
			return nil, "", errGHProfileLogin
		}
		return github.ProfileEnv(prof.ConfigDir), c.login, nil
	case prof.Required():
		return nil, "", errNoGHProfile
	}
	return nil, "", nil
}

// runner runs gh with env.
func (g *ghProfiles) runner(env []string) github.Runner {
	run := github.GHEnv
	if g != nil && g.run != nil {
		run = g.run
	}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return run(ctx, env, args...)
	}
}

// Routed runs each gh call as the login of the GitHub account it is about:
// the owner in a github.com URL or an API path (repos/, users/, orgs/) among
// its arguments. A call naming no account runs as the default login; one
// naming two different accounts is refused.
func (g *ghProfiles) Routed() github.Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		owner, err := callOwner(args)
		if err != nil {
			return nil, err
		}
		var env []string
		if owner != "" {
			if env, err = g.Env(ctx, owner); err != nil {
				return nil, err
			}
		}
		return g.runner(env)(ctx, args...)
	}
}

// callOwner is the GitHub account a gh call's arguments are about, or "".
func callOwner(args []string) (string, error) {
	var owner string
	for _, a := range args {
		o := argOwner(a)
		switch {
		case o == "":
		case owner == "":
			owner = o
		case !strings.EqualFold(owner, o):
			return "", errTwoOwners
		}
	}
	return owner, nil
}

// argOwner is the account one gh argument names: the first path segment of a
// github.com URL, or the segment after repos/, users/ or orgs/ in an API path.
func argOwner(a string) string {
	rest, ok := strings.CutPrefix(a, "https://github.com/")
	if !ok {
		a = strings.TrimPrefix(a, "/")
		for _, p := range []string{"repos/", "users/", "orgs/"} {
			if rest, ok = strings.CutPrefix(a, p); ok {
				break
			}
		}
	}
	if !ok {
		return ""
	}
	owner, _, _ := strings.Cut(rest, "/")
	owner, _, _ = strings.Cut(owner, "?")
	if !github.ValidLogin(owner) {
		return ""
	}
	return owner
}

// account returns the gh environment and login for calls about owner: a
// profile's, or nil and the login def reports for the default one.
func (g *ghProfiles) account(ctx context.Context, owner string, def func(context.Context) (string, error)) ([]string, string, error) {
	var env []string
	var login string
	if g != nil && g.spaces != nil {
		// One read of the policy, so the login and the environment cannot
		// come from different revisions.
		res, err := g.spaces.Current()
		if err != nil {
			return nil, "", err
		}
		if prof, ok := res.GHProfileFor(owner); ok {
			if env, login, err = g.profileAccount(ctx, prof); err != nil {
				return nil, "", err
			}
		}
	}
	if env == nil {
		l, err := def(ctx)
		return nil, l, err
	}
	return env, login, nil
}

// check runs 'gh auth status' as profile directory dir (defaultProfile: the
// default login), and lists the orgs of the account it reports, caching the
// answer.
func (g *ghProfiles) check(ctx context.Context, dir string) profileCheck {
	g.mu.Lock()
	c, ok := g.checked[dir]
	g.mu.Unlock()
	if ok {
		ttl := profileCheckTTL
		if c.err != nil {
			ttl = profileRetryTTL
		}
		if g.now().Sub(c.at) < ttl {
			return c
		}
	}
	// Reason: a caller that gives up early (any task may call Status) must
	// not leave a working profile cached as failed, so the check runs on its
	// own deadline and a caller's cancellation is never cached.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), profileCheckTimeout)
	defer cancel()
	var env []string
	if dir != defaultProfile {
		env = github.ProfileEnv(dir)
	}
	gh := g.runner(env)
	c = profileCheck{at: g.now()}
	if c.login, c.err = github.AuthLogin(cctx, gh); c.err == nil {
		c.orgs, c.err = github.Orgs(cctx, gh)
	}
	if ctx.Err() != nil && c.err != nil {
		return c
	}
	g.mu.Lock()
	g.checked[dir] = c
	g.mu.Unlock()
	return c
}

// sees reports whether c's account is, or belongs to, GitHub account login.
func (c profileCheck) sees(login string) bool {
	return strings.EqualFold(c.login, login) || containsFold(c.orgs, login)
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// problems lists, for the operator, what keeps the spaces' gh profiles from
// separating their GitHub accounts: an isolated space binding orgs with no
// profile, a profile not logged in, and a profile whose account can see an
// org bound to a different isolated space or shares its account with another
// isolated space. Each names the space, so only the operator may see them.
func (g *ghProfiles) problems(ctx context.Context) []string {
	if g == nil || g.spaces == nil {
		return nil
	}
	res, err := g.spaces.Current()
	if err != nil || !res.Configured() {
		return nil
	}
	profs := res.GHProfiles()
	isolatedLogins := map[string]string{}
	var out []string
	// The default login runs every call about an org no profile covers, so
	// it must not reach an isolated space's orgs either.
	if bindsIsolatedOrgs(profs) {
		if def := g.check(ctx, defaultProfile); def.err == nil {
			isolatedLogins[strings.ToLower(def.login)] = "the default gh login"
			for _, q := range profs {
				if q.Kind != spaces.Isolated {
					continue
				}
				for _, org := range q.Orgs {
					if def.sees(org) {
						out = append(out, fmt.Sprintf("the default gh login can see GitHub org %q, bound to isolated space %q, so account separation is not effective", github.Printable(org, 64), q.Space))
					}
				}
			}
		}
	}
	for _, p := range profs {
		if p.ConfigDir == "" {
			if p.Required() {
				out = append(out, fmt.Sprintf("space %q has no gh profile, so its GitHub orgs are not synced; set its gh_config_dir, then run: GH_CONFIG_DIR=<that directory> gh auth login", p.Space))
			}
			continue
		}
		c := g.check(ctx, p.ConfigDir)
		if c.err != nil {
			out = append(out, fmt.Sprintf("space %q's gh profile is not logged in, so its GitHub orgs are not synced; run: GH_CONFIG_DIR=%s gh auth login", p.Space, shellQuote(github.Printable(p.ConfigDir, 1024))))
			continue
		}
		for _, q := range profs {
			if q.Space == p.Space || q.Kind != spaces.Isolated {
				continue
			}
			for _, org := range q.Orgs {
				if c.sees(org) {
					out = append(out, fmt.Sprintf("space %q's gh account can see GitHub org %q, bound to isolated space %q, so account separation is not effective", p.Space, github.Printable(org, 64), q.Space))
				}
			}
		}
		if p.Kind == spaces.Isolated {
			key := strings.ToLower(c.login)
			if other, dup := isolatedLogins[key]; dup {
				if other == "the default gh login" {
					out = append(out, fmt.Sprintf("isolated space %q's gh profile is logged in to the same GitHub account as the default gh login, so account separation is not effective", p.Space))
				} else {
					out = append(out, fmt.Sprintf("isolated spaces %q and %q are logged in to the same GitHub account, so account separation is not effective", other, p.Space))
				}
			}
			isolatedLogins[key] = p.Space
		}
	}
	return out
}

// bindsIsolatedOrgs reports whether any of profs is an isolated space with
// GitHub orgs.
func bindsIsolatedOrgs(profs []spaces.GHProfile) bool {
	for _, p := range profs {
		if p.Kind == spaces.Isolated && len(p.Orgs) > 0 {
			return true
		}
	}
	return false
}

// docsClients returns the GitHub and Git an owner's docs sync uses.
func (g *ghProfiles) docsClients(ctx context.Context, owner string) (docs.GitHub, docs.Git, error) {
	env, err := g.Env(ctx, owner)
	switch {
	case err != nil:
		return nil, nil, err
	case env == nil:
		return docs.NewGitHub(), docs.NewGit(), nil
	}
	return docs.NewProfileGitHub(env), docs.NewProfileGit(env), nil
}

// watchAccounts returns the PR watcher's profile accounts and the route that
// sends each owner's items through its space's account. A policy that does not
// verify is an error, so the sweep keeps its cache rather than run unrouted.
func (g *ghProfiles) watchAccounts(ctx context.Context) ([]prs.Account, func(string) (string, bool), error) {
	if g == nil || g.spaces == nil {
		return nil, nil, nil
	}
	res, err := g.spaces.Current()
	if err != nil || !res.Configured() {
		return nil, nil, err
	}
	var accounts []prs.Account
	usable := map[string]bool{}
	for _, p := range res.GHProfiles() {
		if p.ConfigDir == "" {
			continue
		}
		c := g.check(ctx, p.ConfigDir)
		if c.err != nil {
			continue
		}
		usable[p.Space] = true
		accounts = append(accounts, prs.Account{Name: p.ConfigDir, Login: c.login, GH: g.runner(github.ProfileEnv(p.ConfigDir))})
	}
	route := func(owner string) (string, bool) {
		p, ok := res.GHProfileFor(owner)
		switch {
		case !ok:
			return "", true
		case p.ConfigDir != "" && usable[p.Space]:
			return p.ConfigDir, true
		case p.ConfigDir != "":
			return "", false
		}
		return "", !p.Required()
	}
	return accounts, route, nil
}
