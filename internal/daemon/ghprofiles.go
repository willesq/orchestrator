package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/spaces"
)

// ghProfiles picks the gh login each GitHub call orchd makes runs with
// (SP-063, D-59, SP-074). A call about an org bound to a space that names a gh
// profile runs as that profile's account, and is refused when the profile is
// not usable (not logged in, or not an owner-only directory under the home),
// never run as another login; anything else, an isolated space's org without
// a profile included, runs as the default login. A profile is checked only
// with 'gh auth status' and the account it reports: orch never reads inside a
// profile directory or asks gh for a token.
type ghProfiles struct {
	spaces *spaces.Engine
	// run runs gh with env, nil being the default login; github.GHEnv
	// outside tests.
	run func(ctx context.Context, env []string, args ...string) ([]byte, error)
	now func() time.Time
	// checkDir vets a profile directory before any call runs with it;
	// github.CheckProfileDir outside tests.
	checkDir func(dir string) error
	// budget caps each isolated space's own account's calls an hour.
	budget *ghBudget

	mu      sync.Mutex
	checked map[string]profileCheck
}

// profileCheck is what 'gh auth status' and the account's orgs said about one
// profile directory, and when.
type profileCheck struct {
	login string
	// storage is where gh keeps the login's token (github.Auth.Storage).
	storage string
	orgs    []string
	err     error
	at      time.Time
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
	errGHProfileLogin = errors.New("its space's gh profile is not logged in; the operator can see which in 'orch status'")
	errGHProfileDir   = errors.New("its space's gh profile is not a directory only you can open under your home; the operator can see which in 'orch status'")
	errTwoOwners      = errors.New("the gh call names two GitHub accounts")
)

// newGHProfiles caps each isolated space's own account at budget GitHub API
// calls an hour; 0 is no cap.
func newGHProfiles(budget int) *ghProfiles {
	return &ghProfiles{
		run: github.GHEnv, now: time.Now, checkDir: github.CheckProfileDir,
		budget: newGHBudget(budget, time.Now), checked: map[string]profileCheck{},
	}
}

// defaultProfile keys the default login's check, which has no directory.
const defaultProfile = ""

// profileFor returns the gh profile of the space binding owner; ok is false
// when calls about owner run as the default login. An error refuses the call,
// including when the applied policy does not verify.
func (g *ghProfiles) profileFor(owner string) (spaces.GHProfile, bool, error) {
	if g == nil || g.spaces == nil {
		return spaces.GHProfile{}, false, nil
	}
	res, err := g.spaces.Current()
	if err != nil {
		return spaces.GHProfile{}, false, err
	}
	prof, ok := res.GHProfileFor(owner)
	return prof, ok && prof.ConfigDir != "", nil
}

// Env returns the environment for gh calls about GitHub account owner: nil
// for the default login, or a profile's environment. An error refuses the
// call.
func (g *ghProfiles) Env(ctx context.Context, owner string) ([]string, error) {
	prof, ok, err := g.profileFor(owner)
	if err != nil || !ok {
		return nil, err
	}
	return g.profileEnv(ctx, prof)
}

// profileEnv returns the environment for gh calls as prof.
func (g *ghProfiles) profileEnv(ctx context.Context, prof spaces.GHProfile) ([]string, error) {
	env, _, err := g.profileAccount(ctx, prof)
	return env, err
}

// profileAccount returns the environment and login for gh calls as prof: nil
// and "" for the default login, which a space naming no profile uses (D-59).
func (g *ghProfiles) profileAccount(ctx context.Context, prof spaces.GHProfile) ([]string, string, error) {
	if prof.ConfigDir == "" {
		return nil, "", nil
	}
	c, err := g.usable(ctx, prof.ConfigDir)
	if err != nil {
		return nil, "", err
	}
	return github.ProfileEnv(prof.ConfigDir), c.login, nil
}

// usable checks profile directory dir and its login, refusing one orch must
// not run gh with.
func (g *ghProfiles) usable(ctx context.Context, dir string) (profileCheck, error) {
	if g.checkDir != nil && g.checkDir(dir) != nil {
		return profileCheck{}, errGHProfileDir
	}
	c := g.check(ctx, dir)
	if c.err != nil {
		return c, errGHProfileLogin
	}
	return c, nil
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

// profileRunner runs gh as prof's account. A call its account cannot read
// fails with github.ErrNoAccess and is never retried as another login. Calls
// of an isolated space's account count against its hourly budget, and with
// gated (docs sync, PR watching) are refused once it is spent.
func (g *ghProfiles) profileRunner(prof spaces.GHProfile, env []string, gated bool) github.Runner {
	run := g.runner(env)
	gate := g.gate(prof)
	return func(ctx context.Context, args ...string) ([]byte, error) {
		switch {
		case gate == nil:
		case gated:
			if err := gate(); err != nil {
				return nil, err
			}
		case !g.budget.take(prof.ConfigDir, hardCapFactor):
			return nil, github.ErrQuota
		}
		out, err := run(ctx, args...)
		return out, github.MarkNoAccess(err)
	}
}

// Routed runs each gh call as the login of the GitHub account it is about:
// the owner in a github.com URL or an API path (repos/, users/, orgs/) among
// its arguments. A call naming no account runs as the default login; one
// naming two different accounts is refused.
func (g *ghProfiles) Routed() github.Runner { return g.routed(false) }

// RoutedBackground is Routed for background sweeps (PR outcomes): an
// isolated space's account whose hourly budget is spent is refused.
func (g *ghProfiles) RoutedBackground() github.Runner { return g.routed(true) }

func (g *ghProfiles) routed(gated bool) github.Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		owner, err := callOwner(args)
		if err != nil {
			return nil, err
		}
		var prof spaces.GHProfile
		ok := false
		if owner != "" {
			if prof, ok, err = g.profileFor(owner); err != nil {
				return nil, err
			}
		}
		if !ok {
			return g.runner(nil)(ctx, args...)
		}
		env, err := g.profileEnv(ctx, prof)
		if err != nil {
			return nil, err
		}
		return g.profileRunner(prof, env, gated)(ctx, args...)
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
	var auth github.Auth
	if auth, c.err = github.AuthStatus(cctx, gh); c.err == nil {
		c.login, c.storage = auth.Login, auth.Storage
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

// problems lists, for the operator, what keeps the spaces that name their own
// gh profile from separating their GitHub accounts: a profile directory orch
// will not use, a profile not logged in, a profile whose account can see an
// org bound to a different isolated space, two isolated spaces sharing an
// account, and the default login reaching an isolated space's own account's
// orgs. A space naming no profile uses the default login by design (D-59), so
// it is not a problem. Each names the space, so only the operator may see
// them.
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
	// it must not reach the orgs of an isolated space that has an account of
	// its own.
	if bindsIsolatedOrgs(profs) {
		if def := g.check(ctx, defaultProfile); def.err == nil {
			isolatedLogins[strings.ToLower(def.login)] = "the default gh login"
			for _, q := range profs {
				if q.Kind != spaces.Isolated || q.ConfigDir == "" {
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
			continue
		}
		dir := shellQuote(github.Printable(p.ConfigDir, 1024))
		if g.checkDir != nil {
			if err := g.checkDir(p.ConfigDir); err != nil {
				out = append(out, fmt.Sprintf("space %q's gh profile is refused, so none of its GitHub calls run: %s", p.Space, github.Printable(err.Error(), 1024)))
				continue
			}
		}
		c := g.check(ctx, p.ConfigDir)
		if c.err != nil {
			out = append(out, fmt.Sprintf("space %q's gh profile is not logged in, so its GitHub orgs are not synced; run: GH_CONFIG_DIR=%s gh auth login", p.Space, dir))
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
// GitHub orgs and a gh profile of its own.
func bindsIsolatedOrgs(profs []spaces.GHProfile) bool {
	for _, p := range profs {
		if p.Kind == spaces.Isolated && p.ConfigDir != "" && len(p.Orgs) > 0 {
			return true
		}
	}
	return false
}
