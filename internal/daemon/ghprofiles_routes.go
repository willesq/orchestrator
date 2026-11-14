package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The routes of orchd's GitHub clients through the spaces' gh profiles: the
// GitHub tab's scopes and repo picker (github.AccountRouter), docs sync and
// the PR watcher (SP-063, SP-074).

// Account is github.AccountRouter's: the runner and identity of the space's
// own gh profile for calls about owner, or ok false for the default login.
func (g *ghProfiles) Account(ctx context.Context, owner string) (github.Runner, github.Identity, bool, error) {
	prof, ok, err := g.profileFor(owner)
	if err != nil || !ok {
		return nil, github.Identity{}, false, err
	}
	c, err := g.usable(ctx, prof.ConfigDir)
	if err != nil {
		return nil, github.Identity{}, false, err
	}
	gh := g.profileRunner(prof, github.ProfileEnv(prof.ConfigDir), false)
	return gh, github.Identity{Login: c.login, Orgs: slices.Clone(c.orgs)}, true, nil
}

// ownsProfile reports whether owner is bound to a space that names its own
// gh profile, or the policy cannot be read; such an owner's GitHub data is
// shown to the operator only.
func (g *ghProfiles) ownsProfile(owner string) bool {
	_, own, err := g.profileFor(owner)
	return own || err != nil
}

// refusedProfile reports whether err refused a call because of the space's
// own gh profile: not usable, or its account cannot read the target.
func refusedProfile(err error) bool {
	return errors.Is(err, errGHProfileDir) || errors.Is(err, errGHProfileLogin) || errors.Is(err, github.ErrNoAccess)
}

// ScopeOwners is github.AccountRouter's: each org a space binds that its own
// gh profile's account is or belongs to, so the user can turn on watching an
// org only that profile reaches (SP-074.1).
func (g *ghProfiles) ScopeOwners(ctx context.Context) []github.Scope {
	if g == nil || g.spaces == nil {
		return nil
	}
	res, err := g.spaces.Current()
	if err != nil || !res.Configured() {
		return nil
	}
	var out []github.Scope
	for _, p := range res.GHProfiles() {
		if p.ConfigDir == "" {
			continue
		}
		c, err := g.usable(ctx, p.ConfigDir)
		if err != nil {
			continue
		}
		for _, org := range p.Orgs {
			switch {
			case strings.EqualFold(org, c.login):
				out = append(out, github.Scope{Owner: org, Kind: github.KindUser})
			case containsFold(c.orgs, org):
				out = append(out, github.Scope{Owner: org, Kind: github.KindOrg})
			}
		}
	}
	return out
}

// docsClients returns the GitHub and Git an owner's docs sync uses. An
// isolated space's own account's listing calls count against its hourly
// budget and wait once it is spent (SP-074.2).
func (g *ghProfiles) docsClients(ctx context.Context, owner string) (docs.GitHub, docs.Git, error) {
	prof, ok, err := g.profileFor(owner)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return docs.NewGitHub(), docs.NewGit(), nil
	}
	env, err := g.profileEnv(ctx, prof)
	if err != nil {
		return nil, nil, err
	}
	gate := g.gate(prof)
	if gate != nil && g.budget.spent(prof.ConfigDir) {
		return nil, nil, github.ErrQuota
	}
	return docs.NewGatedProfileGitHub(env, gate), docs.NewProfileGit(env), nil
}

// gate returns the budget check run before each sync or watch call as
// prof's account: nil for an account that is not budgeted.
func (g *ghProfiles) gate(prof spaces.GHProfile) func() error {
	if prof.Kind != spaces.Isolated || g.budget == nil || g.budget.limit == 0 {
		return nil
	}
	return func() error {
		if !g.budget.take(prof.ConfigDir, 1) {
			return github.ErrQuota
		}
		return nil
	}
}

// watchAccounts returns the PR watcher's profile accounts and the route that
// sends each owner's items through its space's account. A policy that does not
// verify is an error, so the sweep keeps its cache rather than run unrouted.
// An owner whose space names a profile orch cannot use is dropped, never
// searched as another login; an isolated space's account whose hourly budget
// is spent is deferred, so its items are kept from the last sweep.
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
		c, err := g.usable(ctx, p.ConfigDir)
		if err != nil {
			continue
		}
		usable[p.Space] = true
		a := prs.Account{Name: p.ConfigDir, Login: c.login, GH: g.profileRunner(p, github.ProfileEnv(p.ConfigDir), true)}
		a.Deferred = g.gate(p) != nil && g.budget.spent(p.ConfigDir)
		accounts = append(accounts, a)
	}
	route := func(owner string) (string, bool) {
		p, ok := res.GHProfileFor(owner)
		switch {
		case !ok || p.ConfigDir == "":
			// D-59: a space naming no profile uses the default login.
			return "", true
		case usable[p.Space]:
			return p.ConfigDir, true
		}
		return "", false
	}
	return accounts, route, nil
}
