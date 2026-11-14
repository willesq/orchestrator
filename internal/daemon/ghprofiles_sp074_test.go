package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/reviews"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
)

// profilesServerDB is profilesServer with orch.db open for the GitHub scope
// store and the task ledger, and orchd's GitHub service routed through the
// profiles as daemon.go wires it.
func profilesServerDB(t *testing.T, doc string) (*server, *ghProfiles, *sql.DB) {
	t.Helper()
	s, ghp := profilesServer(t, doc)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	svc := github.NewService(github.NewStore(conn), ghp.Routed())
	svc.Accounts = ghp
	s.github = svc
	return s, ghp, conn
}

func writeFake(t *testing.T, f *profileGH, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// noDefaultCallsAbout fails for any gh run about owner that did not run as
// profile.
func noDefaultCallsAbout(t *testing.T, f *profileGH, owner, profile string) int {
	t.Helper()
	n := 0
	for _, c := range f.calls(t) {
		if !strings.Contains(strings.ToLower(c.args), owner) {
			continue
		}
		n++
		if c.dir != profile {
			t.Errorf("%q ran with GH_CONFIG_DIR %q, want %q", c.args, c.dir, profile)
		}
	}
	return n
}

const acmeHit = `[{"url":"https://github.com/acme/api/pull/7","title":"Fix","number":7,"updatedAt":"2026-11-14T00:00:00Z",
 "repository":{"nameWithOwner":"acme/api"},"author":{"login":"work-acme"},"assignees":[]}]`

// TestProfileOnlyOrgIsWatchedThroughItsProfile is SP-074.1.a1: an org only a
// space's gh profile belongs to is offered as a GitHub scope, can be turned
// on, and its PRs are found, enriched and reviewed as that profile; no call
// about it runs as the default login.
func TestProfileOnlyOrgIsWatchedThroughItsProfile(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "default", "default-me")
	f.login(t, "acme", "work-acme", "acme")
	writeFake(t, f, "acme.search", acmeHit)
	s, ghp, _ := profilesServerDB(t, profilesDoc)
	ctx := context.Background()

	list, err := s.ListGitHubScopes(operatorCtx, &orchdv1.ListGitHubScopesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var acme *orchdv1.GitHubScope
	for _, sc := range list.GetScopes() {
		if sc.GetOwner() == "acme" {
			acme = sc
		}
		if sc.GetOwner() == "initech" || sc.GetOwner() == "globex" {
			t.Errorf("offered %q, which no usable profile reaches", sc.GetOwner())
		}
	}
	if acme == nil || acme.GetEnabled() || acme.GetKind() != github.KindOrg {
		t.Fatalf("acme scope = %v; want a disabled org scope", acme)
	}
	if _, err := s.SetGitHubScope(operatorCtx, &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
		Owner: "acme", Enabled: true, Items: github.ItemsInvolving, RepoMode: github.ReposAll,
	}}); err != nil {
		t.Fatalf("turning on the profile-only org: %v", err)
	}

	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	bb := prs.New(state, filepath.Join(dir, "events"), watchFrom(s.github, ghp))
	if err := bb.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	pr, ok := prs.LoadState(state).PRs["https://github.com/acme/api/pull/7"]
	if !ok || !pr.Authored() {
		t.Fatalf("the profile's PR was not watched: %+v", prs.LoadState(state).PRs)
	}
	if err := reviews.NewGHPoster(ghp.Env)(ctx, "acme/api", 7, "APPROVE", strings.Repeat("a", 40), "ok"); err != nil {
		t.Fatal(err)
	}
	if n := noDefaultCallsAbout(t, f, "acme", acmeProfile); n < 2 {
		t.Errorf("only %d gh runs about acme", n)
	}
	var searched bool
	for _, c := range f.calls(t) {
		searched = searched || c.dir == acmeProfile && strings.HasPrefix(c.args, "search prs")
	}
	if !searched {
		t.Error("the PR watcher did not search as the profile")
	}
}

// TestProfileCallsItCannotReadAreRefused is SP-074.1.a2: a call a space's
// profile cannot read fails with a clear error and runs once, as the profile
// only; a gh_config_dir that is not an owner-only directory under the home is
// refused before any gh run, and the operator is told why.
func TestProfileCallsItCannotReadAreRefused(t *testing.T) {
	t.Run("no access", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "acme", "work-acme", "acme")
		_, ghp := profilesServer(t, profilesDoc)
		_, err := ghp.Routed()(context.Background(), "api", "repos/acme/nosuch")
		if !errors.Is(err, github.ErrNoAccess) || !strings.Contains(err.Error(), "cannot read") || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v", err)
		}
		var runs []loggedCall
		for _, c := range f.calls(t) {
			if strings.Contains(c.args, "nosuch") {
				runs = append(runs, c)
			}
		}
		if len(runs) != 1 || runs[0].dir != acmeProfile {
			t.Errorf("runs of the refused call = %+v, want one as the profile", runs)
		}
	})

	t.Run("profile directory", func(t *testing.T) {
		f := newProfileGH(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		prof := filepath.Join(home, "acme")
		if err := os.Mkdir(prof, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(prof, 0o755); err != nil {
			t.Fatal(err)
		}
		f.login(t, "acme", "work-acme", "acme")
		doc := `{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"` + prof + `"},
 {"name":"elsewhere","kind":"isolated","github_orgs":["initech"],"gh_config_dir":"/profiles/initech"}]}`
		s, ghp := profilesServer(t, doc)
		ghp.checkDir = github.CheckProfileDir
		ctx := context.Background()
		for _, owner := range []string{"acme", "initech"} {
			if _, err := ghp.Routed()(ctx, "api", "repos/"+owner+"/api"); !errors.Is(err, errGHProfileDir) {
				t.Errorf("%s: err = %v, want the directory refused", owner, err)
			}
			res := docs.SyncOwner(ctx, nil, docs.Owner{Owner: owner}, nil, docs.SyncOpts{RepoCache: t.TempDir(), Clients: ghp.docsClients})
			if res.Status != "error" || !strings.Contains(res.Reason, errGHProfileDir.Error()) {
				t.Errorf("%s docs sync = %+v", owner, res)
			}
		}
		if calls := f.calls(t); len(calls) != 0 {
			t.Errorf("gh ran with a refused profile: %+v", calls)
		}
		op := s.spacesStatus(operatorCtx).GetError()
		for _, want := range []string{`space "acme"'s gh profile is refused`, "chmod 700", `space "elsewhere"'s gh profile is refused`, "does not exist"} {
			if !strings.Contains(op, want) {
				t.Errorf("operator status %q lacks %q", op, want)
			}
		}
		if err := os.Chmod(prof, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ghp.Routed()(ctx, "api", "repos/acme/api"); err != nil {
			t.Errorf("owner-only profile still refused: %v", err)
		}
	})
}

// TestRepoPickerAndPROutcomesUseTheSpacesProfile is SP-074.1.a3: the GitHub
// tab's repo picker and the PR-outcome tracker read an isolated space's repos
// as its profile; the default login is never used for them.
func TestRepoPickerAndPROutcomesUseTheSpacesProfile(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "default", "default-me")
	f.login(t, "acme", "work-acme", "acme")
	writeFake(t, f, "acme.prview", `{"state":"MERGED","createdAt":"`+time.Now().UTC().Format(time.RFC3339)+`","author":{"login":"work-acme"},"commits":[]}`)
	s, ghp, conn := profilesServerDB(t, profilesDoc)
	ctx := context.Background()

	if _, err := s.ListGitHubRepos(operatorCtx, &orchdv1.ListGitHubReposRequest{Owner: "acme"}); err != nil {
		t.Fatalf("repo picker: %v", err)
	}
	if _, err := s.ListGitHubRepos(operatorCtx, &orchdv1.ListGitHubReposRequest{Owner: "initech"}); err == nil {
		t.Error("repo picker listed an org whose profile is not logged in")
	}

	created := time.Now().Add(-time.Hour)
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, prs, created_at)
		VALUES ('t1', 'dev', 'b', '', 'complete', ?, 'code', 'claude-code', 'opus', ?, ?)`,
		time.Now().UTC().Add(time.Minute).Format(time.DateTime), "https://github.com/acme/api/pull/9", created.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := newAttributor(conn, s.github, ghp).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := conn.QueryRow(`SELECT COALESCE(pr_outcome,'') FROM tasks WHERE id = 't1'`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "good" {
		t.Errorf("outcome = %q, want good: the PR is the profile account's own", outcome)
	}
	noDefaultCallsAbout(t, f, "acme", acmeProfile)
	for _, c := range f.calls(t) {
		if c.dir == "" && strings.HasPrefix(c.args, "api user") {
			t.Errorf("asked the default login who it is for an isolated space's PR: %q", c.args)
		}
	}
}

// TestOneLoginForEverySpace is SP-074.3.a1 (D-59): with no space naming a gh
// profile, docs sync, PR watching, reviews, the guard's repo lookups and the
// generated checkout settings all use the default gh login with no
// GH_CONFIG_DIR and nothing to set up; a space that names gh_config_dir uses
// it, never the default, for its orgs.
func TestOneLoginForEverySpace(t *testing.T) {
	const oneLogin = `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["/home/u/code/acme"]},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["/home/u/code/globex"]},
 {"name":"personal","kind":"private","github_orgs":["me"]},
 {"name":"oss","kind":"public","github_orgs":["oss-org"]}
]}`
	f := newProfileGH(t)
	f.login(t, "default", "default-me", "acme", "globex")
	writeFake(t, f, "default.search", acmeHit)
	s, ghp, _ := profilesServerDB(t, oneLogin)
	ctx := context.Background()

	for _, owner := range []string{"acme", "globex", "me", "oss-org"} {
		gh, _, err := ghp.docsClients(ctx, owner)
		if err != nil {
			t.Fatalf("%s docs: %v", owner, err)
		}
		if _, err := gh.ListRepos(ctx, owner); err != nil {
			t.Fatal(err)
		}
		if err := reviews.NewGHPoster(ghp.Env)(ctx, owner+"/api", 1, "APPROVE", strings.Repeat("a", 40), "ok"); err != nil {
			t.Fatalf("%s review: %v", owner, err)
		}
		if r := newRepoLookups(ghp.Routed()).lookup(ctx, owner, "api"); r == nil {
			t.Fatalf("%s repo lookup: nil", owner)
		}
	}
	accounts, route, err := ghp.watchAccounts(ctx)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("watchAccounts = %v, %v; want the default login only", accounts, err)
	}
	for _, owner := range []string{"acme", "globex", "me"} {
		if name, ok := route(owner); name != "" || !ok {
			t.Errorf("route(%s) = %q, %v; want the default login", owner, name, ok)
		}
	}
	if _, err := s.github.Store.Set(ctx, github.Scope{Owner: "acme", Kind: github.KindOrg, Enabled: true, Items: github.ItemsInvolving, RepoMode: github.ReposAll}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := prs.New(state, filepath.Join(dir, "events"), watchFrom(s.github, ghp)).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := prs.LoadState(state).PRs["https://github.com/acme/api/pull/7"]; !ok {
		t.Error("an isolated space's PR was not watched as the default login")
	}
	calls := f.calls(t)
	if len(calls) < 12 {
		t.Fatalf("only %d gh runs", len(calls))
	}
	for _, c := range calls {
		if c.dir != "" {
			t.Errorf("%q ran with GH_CONFIG_DIR %q", c.args, c.dir)
		}
	}
	if e := s.spacesStatus(operatorCtx).GetError(); e != "" {
		t.Errorf("status reports problems for a single login: %q", e)
	}

	// Generated checkout settings set no GH_CONFIG_DIR.
	res, err := ghp.spaces.Current()
	if err != nil {
		t.Fatal(err)
	}
	layout := spacesettings.Layout{Dir: t.TempDir(), GHDefault: "/home/u/.config/gh"}
	for _, name := range []string{"acme", "globex", "personal", "oss"} {
		r := spacesettings.For(res, name, layout)
		if r.GHConfigDir != "" {
			t.Errorf("%s rules carry GH_CONFIG_DIR %q", name, r.GHConfigDir)
		}
		for _, fr := range []spacesettings.Fragment{spacesettings.ClaudeFragment(r), spacesettings.LauncherFragment(r)} {
			if _, ok := fr.Scalars["env.GH_CONFIG_DIR"]; ok {
				t.Errorf("%s settings set GH_CONFIG_DIR", name)
			}
		}
	}

	// A space that names a profile uses it for its orgs, never the default.
	_, ghp2 := profilesServer(t, `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/profiles/acme"},
 {"name":"globex","kind":"isolated","github_orgs":["globex"]}]}`)
	f.login(t, "acme", "work-acme", "acme")
	before := len(f.calls(t))
	if _, err := ghp2.Routed()(ctx, "api", "repos/acme/api"); err != nil {
		t.Fatal(err)
	}
	if _, err := ghp2.Routed()(ctx, "api", "repos/globex/api"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls(t)[before:] {
		switch callAccount(c.args) {
		case "acme":
			if c.dir != acmeProfile {
				t.Errorf("%q ran as %q, want acme's profile", c.args, c.dir)
			}
		case "globex":
			if c.dir != "" {
				t.Errorf("%q ran as %q, want the default login", c.args, c.dir)
			}
		}
	}
}

// TestProfileOwnedGitHubDataIsTheOperators: the scopes and repos of an org a
// space reaches through its own gh profile are shown to the operator only;
// an agent session gets what it gets for an org that is not its user's, so
// it cannot use another job's account or learn its orgs. Turning on a scope
// whose profile is refused fails rather than trusting the stored row.
func TestProfileOwnedGitHubDataIsTheOperators(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "default", "default-me", "oss-org")
	f.login(t, "acme", "work-acme", "acme")
	s, _, _ := profilesServerDB(t, profilesDoc)
	agent := callerCtx(callerAuth{pid: 7, harness: "claude"})

	scopesOf := func(ctx context.Context) []string {
		list, err := s.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, sc := range list.GetScopes() {
			out = append(out, sc.GetOwner())
		}
		return out
	}
	if got := scopesOf(operatorCtx); !slices.Contains(got, "acme") || !slices.Contains(got, "oss-org") {
		t.Errorf("operator scopes = %v", got)
	}
	if got := scopesOf(agent); slices.Contains(got, "acme") || !slices.Contains(got, "oss-org") {
		t.Errorf("agent scopes = %v; want acme hidden", got)
	}
	before := len(f.calls(t))
	_, err := s.ListGitHubRepos(agent, &orchdv1.ListGitHubReposRequest{Owner: "acme"})
	if grpcstatus.Code(err) != codes.NotFound || !strings.Contains(err.Error(), github.ErrNotMember.Error()) {
		t.Errorf("agent repo listing: err = %v", err)
	}
	if len(f.calls(t)) != before {
		t.Error("an agent's repo listing ran gh as another job's account")
	}
	if _, err := s.ListGitHubRepos(operatorCtx, &orchdv1.ListGitHubReposRequest{Owner: "acme"}); err != nil {
		t.Errorf("operator repo listing: %v", err)
	}

	// The PR cache: an agent sees no item of acme.
	s.prStateFile = filepath.Join(t.TempDir(), "prs.json")
	if err := prs.SaveState(s.prStateFile, prs.State{PRs: map[string]prs.PR{
		"https://github.com/acme/api/pull/1":    {URL: "https://github.com/acme/api/pull/1", Repo: "acme/api", Kind: prs.KindPR},
		"https://github.com/oss-org/lib/pull/2": {URL: "https://github.com/oss-org/lib/pull/2", Repo: "oss-org/lib", Kind: prs.KindPR},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ctx  context.Context
		want int
	}{{operatorCtx, 2}, {agent, 1}} {
		got, err := s.ListPRs(c.ctx, &orchdv1.ListPRsRequest{})
		if err != nil || len(got.GetPrs()) != c.want {
			t.Errorf("ListPRs = %v, %v; want %d", got.GetPrs(), err, c.want)
		}
		items, err := s.ListGitHubItems(c.ctx, &orchdv1.ListGitHubItemsRequest{})
		if err != nil || len(items.GetItems()) != c.want {
			t.Errorf("ListGitHubItems = %v, %v; want %d", items.GetItems(), err, c.want)
		}
		for _, it := range items.GetItems() {
			if c.want == 1 && strings.HasPrefix(it.GetRepo(), "acme/") {
				t.Errorf("an agent saw %s", it.GetUrl())
			}
		}
	}

	if _, err := s.github.Store.Set(context.Background(), github.Scope{Owner: "initech", Kind: github.KindOrg, Items: github.ItemsInvolving, RepoMode: github.ReposAll}); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetGitHubScope(operatorCtx, &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
		Owner: "initech", Enabled: true, Items: github.ItemsInvolving, RepoMode: github.ReposAll}})
	if grpcstatus.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("enabling a scope whose profile is refused: err = %v", err)
	}
}
