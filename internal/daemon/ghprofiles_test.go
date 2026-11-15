package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/reviews"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// profileGH is a fake gh on PATH. Each run appends a line to its log with the
// GH_CONFIG_DIR it saw, its arguments, and the names (never the values) of
// its environment variables. 'gh auth status' answers with the login in
// <dir>/<profile>.login, where <profile> is GH_CONFIG_DIR's last element, or
// with no login; 'user/orgs' with <profile>.orgs.
type profileGH struct {
	dir, log string
}

func newProfileGH(t *testing.T) *profileGH {
	t.Helper()
	f := &profileGH{dir: t.TempDir()}
	f.log = filepath.Join(f.dir, "calls.log")
	script := `#!/bin/sh
d='` + f.dir + `'
{ printf 'dir=%s|args=%s|names=' "$GH_CONFIG_DIR" "$*"; env | cut -d= -f1 | tr '\n' ' '; echo; } >> "$d/calls.log"
cat >/dev/null
p=$(basename "${GH_CONFIG_DIR:-default}")
case "$*" in
  *nosuch*) echo 'HTTP 404: Not Found (https://api.github.com/repos/acme/nosuch)' >&2; exit 1 ;;
esac
case "$*" in
  "auth status"*)
    if [ -f "$d/$p.login" ]; then
      printf '{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"%s","tokenSource":"%s"}]}}' "$(cat "$d/$p.login")" "$(cat "$d/$p.storage" 2>/dev/null || echo keyring)"
    else
      echo '{"hosts":{}}'
    fi ;;
  *user/orgs*) cat "$d/$p.orgs" 2>/dev/null ;;
  "api users/"*) l=${2#users/}; printf '{"login":"%s","node_id":"N_%s"}' "$l" "$l" ;;
  "api repos/"*) printf '{"owner":"%s","name":"api","private":true}' "$(echo "$2" | cut -d/ -f2)" ;;
  "api graphql"*) echo '{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}' ;;
  "search prs"*) cat "$d/$p.search" 2>/dev/null || echo '[]' ;;
  search*) echo '[]' ;;
  "pr view"*"state,createdAt"*) cat "$d/$p.prview" 2>/dev/null || echo '{}' ;;
  "api user"*) echo default-me ;;
  *) echo '{}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CONFIG_DIR", "")
	return f
}

// login logs profile in as login, belonging to orgs.
func (f *profileGH) login(t *testing.T, profile, login string, orgs ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, profile+".login"), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, profile+".orgs"), []byte(strings.Join(orgs, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

type loggedCall struct {
	dir, args string
	names     []string
}

func (f *profileGH) calls(t *testing.T) []loggedCall {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []loggedCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("bad log line %q", line)
		}
		out = append(out, loggedCall{
			dir:   strings.TrimPrefix(parts[0], "dir="),
			args:  strings.TrimPrefix(parts[1], "args="),
			names: strings.Fields(strings.TrimPrefix(parts[2], "names=")),
		})
	}
	return out
}

const (
	acmeProfile    = "/profiles/acme"
	initechProfile = "/profiles/initech"
)

// profilesDoc: acme has a profile, globex none, initech one that is not
// logged in; personal and oss use the default login.
const profilesDoc = `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/profiles/acme"},
 {"name":"globex","kind":"isolated","github_orgs":["globex"]},
 {"name":"initech","kind":"isolated","github_orgs":["initech"],"gh_config_dir":"/profiles/initech"},
 {"name":"personal","kind":"private","github_orgs":["me"]},
 {"name":"oss","kind":"public","github_orgs":["oss-org"]}
]}`

// profilesServer applies doc and returns a server whose gh profiles run the
// fake gh.
func profilesServer(t *testing.T, doc string) (*server, *ghProfiles) {
	t.Helper()
	s := spacesServer(t)
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	ghp := newGHProfiles(0)
	ghp.spaces = s.spaces
	// Reason: these profiles are fake paths; TestGHProfileDirIsChecked
	// covers the directory check.
	ghp.checkDir = nil
	s.ghProfiles = ghp
	return s, ghp
}

// callAccount is the GitHub account a logged call was about: from its URL or
// API path, a GraphQL owner, or a search's --owner, or "" for none.
func callAccount(args string) string {
	for _, a := range strings.Fields(args) {
		if o := argOwner(a); o != "" {
			return o
		}
		for _, p := range []string{"owner=", "--owner="} {
			if o, ok := strings.CutPrefix(a, p); ok {
				return o
			}
		}
	}
	return ""
}

// TestGHProfilesRunEveryCallAsItsSpace is SP-063.1.a1: every gh run orchd
// makes about a space's org (repo and PR lookups, review posting, docs
// listing, org identity checks, PR searches) runs with that space's
// GH_CONFIG_DIR and without token variables that would override it; calls
// about orgs of spaces without a profile (an isolated one included, D-59), or
// about nobody's org, run as the default login; and every run is checked.
func TestGHProfilesRunEveryCallAsItsSpace(t *testing.T) {
	const secret = "fake-token-value-123"
	f := newProfileGH(t)
	t.Setenv("GH_TOKEN", secret)
	f.login(t, "acme", "work-acme", "acme")
	_, ghp := profilesServer(t, profilesDoc)
	ctx := context.Background()
	routed := ghp.Routed()

	// Calls through the routed runner, as reviews, landing, placement and
	// the GitHub tab make them.
	for _, args := range [][]string{
		{"pr", "view", "https://github.com/acme/api/pull/1", "--json", "state"},
		{"api", "repos/acme/api/pulls/1", "--jq", ".author_association"},
		{"api", "orgs/acme/repos?per_page=100"},
		{"pr", "view", "https://github.com/me/notes/pull/2", "--json", "state"},
		{"pr", "view", "https://github.com/globex/web/pull/5", "--json", "state"},
		{"api", "repos/oss-org/lib"},
		{"api", "repos/unbound/x"},
		{"api", "user", "--jq", ".login"},
	} {
		if _, err := routed(ctx, args...); err != nil {
			t.Fatalf("routed %v: %v", args, err)
		}
	}
	if _, err := reviews.FetchDetail(ctx, routed, "https://github.com/acme/api/pull/3"); err != nil {
		t.Logf("FetchDetail (answer not needed): %v", err)
	}
	if _, err := github.LookupRepo(ctx, routed, "acme", "api"); err != nil {
		t.Fatal(err)
	}
	// Review posting.
	if err := reviews.NewGHPoster(ghp.Env)(ctx, "acme/api", 4, "APPROVE", strings.Repeat("a", 40), "ok"); err != nil {
		t.Fatal(err)
	}
	// Docs listing.
	for _, owner := range []string{"acme", "oss-org"} {
		gh, _, err := ghp.docsClients(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gh.ListRepos(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	// Org identity checks.
	res, err := ghp.spaces.Current()
	if err != nil {
		t.Fatal(err)
	}
	prof, _ := res.GHProfileFor("acme")
	if _, err := spacesEnv(ghp).LookupOrg(ctx, "acme", prof); err != nil {
		t.Fatal(err)
	}
	// PR searches as the profile.
	accounts, _, err := ghp.watchAccounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].Login != "work-acme" {
		t.Fatalf("watchAccounts = %+v, %v", accounts, err)
	}
	if _, err := accounts[0].GH(ctx, "search", "prs", "--owner=acme"); err != nil {
		t.Fatal(err)
	}

	calls := f.calls(t)
	if len(calls) < 15 {
		t.Fatalf("only %d gh runs logged", len(calls))
	}
	checked := 0
	for _, c := range calls {
		account := strings.ToLower(callAccount(c.args))
		profileCheck := strings.HasPrefix(c.args, "auth status") || strings.Contains(c.args, "user/orgs")
		hasToken := false
		for _, n := range c.names {
			hasToken = hasToken || n == "GH_TOKEN"
		}
		switch {
		case profileCheck && c.dir == initechProfile:
			// Checking initech's profile is the one call made as it; it is
			// not logged in, so nothing else runs as it.
			if !strings.HasPrefix(c.args, "auth status") || hasToken {
				t.Errorf("%q ran as initech's profile", c.args)
			}
		case account == "acme" || profileCheck:
			if c.dir != acmeProfile || hasToken {
				t.Errorf("%q ran with GH_CONFIG_DIR %q (GH_TOKEN set: %v), want %q and none", c.args, c.dir, hasToken, acmeProfile)
			}
		case account == "initech":
			t.Errorf("%q ran for a space whose profile is not logged in", c.args)
		default:
			// Reason: globex, personal and oss name no profile and unbound
			// accounts no space; their calls keep the default login (D-59).
			if c.dir != "" {
				t.Errorf("%q ran with GH_CONFIG_DIR %q, want the default login", c.args, c.dir)
			}
		}
		checked++
	}
	if checked != len(calls) {
		t.Fatalf("checked %d of %d runs", checked, len(calls))
	}
	t.Logf("checked %d of %d gh runs", checked, len(calls))
	if b, _ := os.ReadFile(f.log); strings.Contains(string(b), secret) {
		t.Error("the log recorded a credential value")
	}
}

// TestGHProfilesIsolatedSpaceWithoutLoginIsNotSynced is SP-063.1.a2 as D-59
// amends it (SP-074.3.a1): a space whose profile is not logged in gets no gh
// call (its docs are not synced, its PRs are dropped, its calls are refused)
// and the operator's status names it with the login command, while other
// callers see only a count. An isolated space naming no profile uses the
// default login and is no problem.
func TestGHProfilesIsolatedSpaceWithoutLoginIsNotSynced(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "acme", "work-acme", "acme")
	s, ghp := profilesServer(t, profilesDoc)
	ctx := context.Background()

	const owner = "initech"
	// A refused owner returns before the registry is read.
	res := docs.SyncOwner(ctx, nil, docs.Owner{Owner: owner}, nil, docs.SyncOpts{RepoCache: t.TempDir(), Clients: ghp.docsClients})
	if res.Status != "error" || !strings.Contains(res.Reason, errGHProfileLogin.Error()) || strings.Contains(res.Reason, owner) {
		t.Errorf("%s docs sync = %+v", owner, res)
	}
	if _, err := ghp.Routed()(ctx, "pr", "view", "https://github.com/"+owner+"/x/pull/1"); !errors.Is(err, errGHProfileLogin) {
		t.Errorf("%s routed call: err = %v", owner, err)
	}
	if err := reviews.NewGHPoster(ghp.Env)(ctx, owner+"/x", 1, "APPROVE", strings.Repeat("a", 40), "ok"); !errors.Is(err, errGHProfileLogin) {
		t.Errorf("%s review post: err = %v", owner, err)
	}
	// globex names no profile: the default login.
	if env, err := ghp.Env(ctx, "globex"); env != nil || err != nil {
		t.Errorf("globex Env = %v, %v; want the default login", env, err)
	}
	_, route, err := ghp.watchAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for owner, want := range map[string]struct {
		account string
		ok      bool
	}{"acme": {acmeProfile, true}, "globex": {"", true}, "initech": {"", false}, "me": {"", true}, "unbound": {"", true}} {
		if a, ok := route(owner); a != want.account || ok != want.ok {
			t.Errorf("route(%s) = %q, %v; want %q, %v", owner, a, ok, want.account, want.ok)
		}
	}
	for _, c := range f.calls(t) {
		if c.dir == initechProfile && !strings.HasPrefix(c.args, "auth status") {
			t.Errorf("ran %q as initech's profile, which is not logged in", c.args)
		}
		if a := strings.ToLower(callAccount(c.args)); a == "initech" {
			t.Errorf("ran %q for a space without a working profile", c.args)
		}
	}

	op := s.spacesStatus(operatorCtx).GetError()
	for _, want := range []string{
		`space "initech"'s gh profile is not logged in`, "run: GH_CONFIG_DIR='/profiles/initech' gh auth login",
	} {
		if !strings.Contains(op, want) {
			t.Errorf("operator status %q lacks %q", op, want)
		}
	}
	for _, unwanted := range []string{`space "acme"`, `globex`} {
		if strings.Contains(op, unwanted) {
			t.Errorf("operator status reports %s: %q", unwanted, op)
		}
	}
	agent := s.spacesStatus(callerCtx(callerAuth{pid: 7, harness: "claude"})).GetError()
	if !strings.Contains(agent, "1 problem(s) with the spaces' GitHub accounts") || strings.Contains(agent, "initech") {
		t.Errorf("agent status = %q", agent)
	}

	// Apply looks orgs up as their space's profile too, so it is refused
	// for a profile that is not logged in, naming the space for the
	// operator, and runs as the default login for a space naming none.
	prof := spaces.GHProfile{Space: "initech", Kind: spaces.Isolated, ConfigDir: initechProfile}
	if _, err := spacesEnv(ghp).LookupOrg(ctx, "initech", prof); err == nil || !strings.Contains(err.Error(), `space "initech"`) {
		t.Errorf("LookupOrg with a logged-out profile: err = %v", err)
	}
	if _, err := spacesEnv(ghp).LookupOrg(ctx, "globex", spaces.GHProfile{Space: "globex", Kind: spaces.Isolated}); err != nil {
		t.Errorf("LookupOrg without a profile: err = %v", err)
	}
}

// TestGHProfilesWarnWhenSeparationIsNotEffective is SP-063.1.a3: the
// operator's status warns when a profile's account can see an org bound to a
// different isolated space, or two isolated spaces share one account.
func TestGHProfilesWarnWhenSeparationIsNotEffective(t *testing.T) {
	const doc = `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/profiles/acme"},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"gh_config_dir":"/profiles/globex"},
 {"name":"personal","kind":"private","github_orgs":["me"],"gh_config_dir":"/profiles/personal"}
]}`
	t.Run("separate accounts", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "acme", "work-acme", "acme")
		f.login(t, "globex", "work-globex", "globex")
		f.login(t, "personal", "me")
		s, _ := profilesServer(t, doc)
		if e := s.spacesStatus(operatorCtx).GetError(); e != "" {
			t.Errorf("status = %q, want no problems", e)
		}
	})
	t.Run("an account sees another isolated space's org", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "acme", "work-acme", "acme", "Globex")
		f.login(t, "globex", "work-globex", "globex")
		f.login(t, "personal", "me", "acme")
		s, _ := profilesServer(t, doc)
		e := s.spacesStatus(operatorCtx).GetError()
		for _, want := range []string{
			`space "acme"'s gh account can see GitHub org "globex", bound to isolated space "globex", so account separation is not effective`,
			`space "personal"'s gh account can see GitHub org "acme", bound to isolated space "acme"`,
		} {
			if !strings.Contains(e, want) {
				t.Errorf("status %q lacks %q", e, want)
			}
		}
		if strings.Contains(e, `space "globex"'s gh account`) {
			t.Errorf("status warns about globex, which sees only its own org: %q", e)
		}
	})
	t.Run("two isolated spaces share an account", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "acme", "work-me", "acme")
		f.login(t, "globex", "Work-Me", "globex")
		f.login(t, "personal", "me")
		s, _ := profilesServer(t, doc)
		if e := s.spacesStatus(operatorCtx).GetError(); !strings.Contains(e, `isolated spaces "acme" and "globex" are logged in to the same GitHub account`) {
			t.Errorf("status = %q", e)
		}
	})
}

func TestCallOwner(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		err  error
	}{
		{args: []string{"pr", "view", "https://github.com/Acme/api/pull/1"}, want: "Acme"},
		{args: []string{"api", "/repos/acme/api"}, want: "acme"},
		{args: []string{"api", "users/acme", "--jq", "{login, node_id}"}, want: "acme"},
		{args: []string{"api", "orgs/acme?x=1"}, want: "acme"},
		{args: []string{"api", "repos/acme/a", "https://github.com/ACME/b"}, want: "acme"},
		{args: []string{"api", "user", "--jq", ".login"}},
		{args: []string{"api", "repos/-bad-/x"}},
		{args: []string{"api", "repos/acme/a", "https://github.com/globex/b"}, err: errTwoOwners},
	} {
		got, err := callOwner(tc.args)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("callOwner(%v) = %q, %v; want %q, %v", tc.args, got, err, tc.want, tc.err)
		}
	}
}

// TestGHProfilesWithoutSpaces: with no spaces engine, or no policy applied,
// every call runs as the default login and nothing is checked.
func TestGHProfilesWithoutSpaces(t *testing.T) {
	ctx := context.Background()
	var none *ghProfiles
	if env, err := none.Env(ctx, "acme"); env != nil || err != nil {
		t.Errorf("nil Env = %v, %v", env, err)
	}
	if p := none.problems(ctx); p != nil {
		t.Errorf("nil problems = %v", p)
	}
	if a, r, err := none.watchAccounts(ctx); a != nil || r != nil || err != nil {
		t.Errorf("nil watchAccounts = %v, %v", a, err)
	}
	s := spacesServer(t)
	ghp := newGHProfiles(0)
	ghp.spaces = s.spaces
	var gotEnv []string
	ghp.run = func(_ context.Context, env []string, _ ...string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}
	if _, err := ghp.Routed()(ctx, "api", "repos/acme/x"); err != nil || gotEnv != nil {
		t.Errorf("unconfigured routed call: env %v, err %v", gotEnv, err)
	}
	gh, git, err := ghp.docsClients(ctx, "acme")
	if err != nil || gh == nil || git == nil {
		t.Errorf("docsClients = %v, %v, %v", gh, git, err)
	}
	if p := ghp.problems(ctx); p != nil {
		t.Errorf("problems = %v", p)
	}
}

// TestGHProfileCheckIgnoresACallersCancellation: a caller that gives up (any
// task may call Status) cannot leave a working profile cached as failed.
func TestGHProfileCheckIgnoresACallersCancellation(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "acme", "work-acme", "acme")
	_, ghp := profilesServer(t, profilesDoc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := ghp.check(ctx, acmeProfile); c.err != nil || c.login != "work-acme" {
		t.Errorf("check with a cancelled caller = %+v", c)
	}
	if env, err := ghp.Env(context.Background(), "acme"); err != nil || env == nil {
		t.Errorf("Env after a cancelled caller = %v, %v", env, err)
	}
	// A failure that is the caller's own cancellation is not cached.
	ghp.run = func(ctx context.Context, _ []string, _ ...string) ([]byte, error) { return nil, context.Canceled }
	ghp.checked = map[string]profileCheck{}
	if c := ghp.check(ctx, acmeProfile); c.err == nil {
		t.Fatal("check succeeded without gh")
	}
	if _, cached := ghp.checked[acmeProfile]; cached {
		t.Error("a cancelled caller's failure was cached")
	}
}

// TestGHProfilesWarnAboutTheDefaultLogin: the default login runs every call
// no profile covers, so status warns when it can see the org of an isolated
// space that names its own profile, or is the same account as one; an
// isolated space naming no profile uses the default login by design (D-59).
func TestGHProfilesWarnAboutTheDefaultLogin(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "default", "work-acme", "globex", "acme")
	f.login(t, "acme", "work-acme", "acme")
	s, _ := profilesServer(t, profilesDoc)
	e := s.spacesStatus(operatorCtx).GetError()
	for _, want := range []string{
		`the default gh login can see GitHub org "acme", bound to isolated space "acme"`,
		`isolated space "acme"'s gh profile is logged in to the same GitHub account as the default gh login`,
	} {
		if !strings.Contains(e, want) {
			t.Errorf("status %q lacks %q", e, want)
		}
	}
	if strings.Contains(e, `"globex"`) {
		t.Errorf("status warns about globex, which names no profile: %q", e)
	}
}

// TestGHProfilesAccountAndLanding: landing's account is the owner's profile
// login and environment, the default identity for anyone else (an isolated
// space naming no profile included, D-59), and refused for a profile that is
// not logged in.
func TestGHProfilesAccountAndLanding(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "acme", "work-acme", "acme")
	_, ghp := profilesServer(t, profilesDoc)
	svc := newLandingService(nil, nil, nil, ghp)
	svc.Login = func(context.Context) (string, error) { return "default-me", nil }
	ctx := context.Background()
	a, err := svc.AccountFor(ctx, "acme")
	if err != nil || a.Login != "work-acme" || !strings.Contains(strings.Join(a.Env, "\n"), "GH_CONFIG_DIR="+acmeProfile) {
		t.Errorf("acme account = %q, env set %v, %v", a.Login, a.Env != nil, err)
	}
	if a, err := svc.AccountFor(ctx, "me"); err != nil || a.Login != "default-me" || a.Env != nil {
		t.Errorf("personal account = %+v, %v", a, err)
	}
	if a, err := svc.AccountFor(ctx, "globex"); err != nil || a.Login != "default-me" || a.Env != nil {
		t.Errorf("globex account = %+v, %v; want the default login (D-59)", a, err)
	}
	if _, err := svc.AccountFor(ctx, "initech"); !errors.Is(err, errGHProfileLogin) {
		t.Errorf("initech account: err = %v", err)
	}
	if svc := newLandingService(nil, nil, nil, nil); svc.AccountFor != nil {
		t.Error("landing without profiles picks accounts")
	}
}

// TestListDocsOwnerReposAsTheSpacesAccount: the docs owner picker lists a
// space's org as its account, and refuses one whose profile is not logged
// in, rather than list it as the default login.
func TestListDocsOwnerReposAsTheSpacesAccount(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "acme", "work-acme", "acme")
	s, _ := profilesServer(t, profilesDoc)
	s.docsGH = docs.NewGitHub()
	if _, err := s.ListDocsOwnerRepos(appOperatorCtx, &orchdv1.ListDocsOwnerReposRequest{Owner: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDocsOwnerRepos(appOperatorCtx, &orchdv1.ListDocsOwnerReposRequest{Owner: "initech"}); err == nil {
		t.Error("listed an org whose profile is not logged in")
	}
	for _, c := range f.calls(t) {
		if strings.HasPrefix(c.args, "api graphql") {
			if a := callAccount(c.args); a != "acme" || c.dir != acmeProfile {
				t.Errorf("listed %q as %q", a, c.dir)
			}
		}
	}
}
