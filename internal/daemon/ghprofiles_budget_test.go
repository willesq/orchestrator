package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prs"
)

func TestGHBudgetWindow(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	b := newGHBudget(2, func() time.Time { return now })
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	if b.spent("/p") {
		t.Fatal("spent before any call")
	}
	b.spend("/p")
	b.spend("/p")
	b.spend("/q")
	if !b.spent("/p") || !b.spent("/p") || b.spent("/q") {
		t.Fatal("budget counted wrong")
	}
	if n := strings.Count(logs.String(), "used its 2 GitHub API calls"); n != 1 {
		t.Errorf("logged %d times, want once an hour: %q", n, logs.String())
	}
	if strings.Contains(logs.String(), "/p") {
		t.Errorf("the log names the account's directory: %q", logs.String())
	}
	now = now.Add(time.Hour)
	if b.spent("/p") {
		t.Error("the budget did not renew after an hour")
	}
	// Calls that are not deferred stop at twice the budget.
	now = now.Add(time.Hour)
	for i := range 4 {
		if !b.take("/r", hardCapFactor) {
			t.Fatalf("call %d refused below the hard cap", i+1)
		}
	}
	if b.take("/r", hardCapFactor) || b.take("/r", 1) {
		t.Error("a call past the hard cap ran")
	}
	var none *ghBudget
	none.spend("/p")
	if !none.take("/p", 1) {
		t.Error("no budget refused a call")
	}
	if none.spent("/p") || newGHBudget(0, time.Now).spent("/p") || newGHBudget(-5, time.Now).limit != 0 {
		t.Error("no budget capped calls")
	}
}

// TestIsolatedAccountBudget is SP-074.2.a1: each isolated space's own account
// has an hourly GitHub API budget; once it is spent, that account's docs sync
// and PR watching wait (logged) while reviews still run, and other accounts
// (a private space's profile, the default login) are not affected. The
// budget renews after the hour.
func TestIsolatedAccountBudget(t *testing.T) {
	const doc = `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/profiles/acme"},
 {"name":"personal","kind":"private","github_orgs":["me"],"gh_config_dir":"/profiles/personal"},
 {"name":"globex","kind":"isolated","github_orgs":["globex"]}
]}`
	f := newProfileGH(t)
	f.login(t, "acme", "work-acme", "acme")
	f.login(t, "personal", "me")
	_, ghp := profilesServer(t, doc)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	ghp.budget = newGHBudget(3, func() time.Time { return now })
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	ctx := context.Background()

	// Three calls as acme spend its hour: reviews and lookups count too.
	for range 3 {
		if _, err := ghp.Routed()(ctx, "api", "repos/acme/api"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ghp.docsClients(ctx, "acme"); !errors.Is(err, github.ErrQuota) {
		t.Errorf("acme docs sync: err = %v, want the budget spent", err)
	}
	res := docs.SyncOwner(ctx, nil, docs.Owner{Owner: "acme"}, nil, docs.SyncOpts{RepoCache: t.TempDir(), Clients: ghp.docsClients})
	if res.Status != "error" || !strings.Contains(res.Reason, "budget is spent") {
		t.Errorf("acme docs sync = %+v", res)
	}
	accounts, _, err := ghp.watchAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]prs.Account{}
	for _, a := range accounts {
		byName[a.Name] = a
	}
	if !byName["/profiles/acme"].Deferred || byName["/profiles/personal"].Deferred {
		t.Errorf("accounts = %+v; want acme deferred, personal not", accounts)
	}
	if _, err := byName["/profiles/acme"].GH(ctx, "search", "prs", "--involves=@me"); !errors.Is(err, github.ErrQuota) {
		t.Errorf("acme watch call: err = %v, want the budget spent", err)
	}
	// Reviews and lookups the user waits on still run, up to twice the
	// budget; background sweeps (PR outcomes) wait.
	if _, err := ghp.RoutedBackground()(ctx, "pr", "view", "https://github.com/acme/api/pull/1"); !errors.Is(err, github.ErrQuota) {
		t.Errorf("acme PR-outcome call: err = %v, want the budget spent", err)
	}
	for i := range 3 {
		if _, err := ghp.Routed()(ctx, "api", "repos/acme/api/pulls/1"); err != nil {
			t.Errorf("acme review call %d refused: %v", i+1, err)
		}
	}
	if _, err := ghp.Routed()(ctx, "api", "repos/acme/api/pulls/1"); !errors.Is(err, github.ErrQuota) {
		t.Errorf("acme review call past twice the budget: err = %v", err)
	}
	// Other accounts are not counted.
	for range 5 {
		for _, owner := range []string{"me", "globex", "unbound"} {
			if _, err := ghp.Routed()(ctx, "api", "repos/"+owner+"/x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, owner := range []string{"me", "globex"} {
		if _, _, err := ghp.docsClients(ctx, owner); err != nil {
			t.Errorf("%s docs sync: %v", owner, err)
		}
	}
	if _, err := byName["/profiles/personal"].GH(ctx, "search", "prs", "--involves=@me"); err != nil {
		t.Errorf("personal watch call: %v", err)
	}
	var budgetLine string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "GitHub API calls for this hour") {
			budgetLine = l
		}
	}
	if !strings.Contains(budgetLine, "used its 3 GitHub API calls") || strings.Contains(budgetLine, "acme") {
		t.Errorf("log = %q; want the spent budget, naming no space", logs.String())
	}

	// A new hour renews it.
	now = now.Add(time.Hour)
	gh, _, err := ghp.docsClients(ctx, "acme")
	if err != nil {
		t.Fatalf("acme docs sync after the hour: %v", err)
	}
	if _, err := gh.ListRepos(ctx, "acme"); err != nil {
		t.Fatal(err)
	}

	// The docs listing's own calls count and stop once spent.
	ghp.budget = newGHBudget(1, func() time.Time { return now })
	gh, _, err = ghp.docsClients(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.ListRepos(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := gh.ListRepos(ctx, "acme"); !errors.Is(err, github.ErrQuota) {
		t.Errorf("second listing: err = %v, want the budget spent", err)
	}
}

// TestPRWatchDefersASpentAccount: the watcher wired to orchd's profiles keeps
// a spent account's items and refreshes the rest (SP-074.2.a1).
func TestPRWatchDefersASpentAccount(t *testing.T) {
	f := newProfileGH(t)
	f.login(t, "default", "default-me")
	f.login(t, "acme", "work-acme", "acme")
	writeFake(t, f, "acme.search", acmeHit)
	s, ghp, _ := profilesServerDB(t, profilesDoc)
	ghp.budget = newGHBudget(100, time.Now)
	if _, _, err := s.github.Scopes(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.github.Store.Set(context.Background(), github.Scope{Owner: "acme", Kind: github.KindOrg, Enabled: true, Items: github.ItemsInvolving, RepoMode: github.ReposAll}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	bb := prs.New(state, filepath.Join(dir, "events"), watchFrom(s.github, ghp))
	ctx := context.Background()
	if err := bb.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	ghp.budget = newGHBudget(1, time.Now)
	ghp.budget.spend(acmeProfile)
	before := len(f.calls(t))
	if err := bb.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := prs.LoadState(state).PRs["https://github.com/acme/api/pull/7"]; !ok {
		t.Error("a spent account's PR was dropped from the cache")
	}
	for _, c := range f.calls(t)[before:] {
		if c.dir == acmeProfile && !strings.HasPrefix(c.args, "auth status") && !strings.Contains(c.args, "user/orgs") {
			t.Errorf("ran %q as a spent account", c.args)
		}
	}
}
