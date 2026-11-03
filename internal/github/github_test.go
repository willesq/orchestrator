package github

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st := NewStore(conn)
	st.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return st
}

func TestValidate(t *testing.T) {
	ok := Scope{Owner: "WippData", Kind: KindOrg, Items: ItemsInvolving, RepoMode: ReposAll}
	tests := []struct {
		name    string
		mutate  func(*Scope)
		wantErr string
	}{
		{"valid org", func(*Scope) {}, ""},
		{"valid include", func(s *Scope) { s.RepoMode, s.Repos = ReposInclude, []string{"api", "web.site_2"} }, ""},
		{"bad login", func(s *Scope) { s.Owner = "bad--login" }, "invalid GitHub login"},
		{"login too long", func(s *Scope) { s.Owner = strings.Repeat("a", 40) }, "invalid GitHub login"},
		{"leading hyphen", func(s *Scope) { s.Owner = "-x" }, "invalid GitHub login"},
		{"39 characters is fine", func(s *Scope) { s.Owner = strings.Repeat("a", 39) }, ""},
		{"hyphen steps past 39", func(s *Scope) { s.Owner = "a" + strings.Repeat("-b", 38) }, "invalid GitHub login"},
		{"dot repo", func(s *Scope) { s.RepoMode, s.Repos = ReposInclude, []string{"."} }, "invalid repo name"},
		{"dotdot repo", func(s *Scope) { s.RepoMode, s.Repos = ReposInclude, []string{".."} }, "invalid repo name"},
		{"bad kind", func(s *Scope) { s.Kind = "team" }, "kind must be"},
		{"bad items", func(s *Scope) { s.Items = "some" }, "items must be"},
		{"bad repo mode", func(s *Scope) { s.RepoMode = "pick" }, "repo mode must be"},
		{"all with repos", func(s *Scope) { s.Repos = []string{"api"} }, "takes no repos"},
		{"include without repos", func(s *Scope) { s.RepoMode = ReposInclude }, "needs at least one repo"},
		{"bad repo name", func(s *Scope) { s.RepoMode, s.Repos = ReposExclude, []string{"../etc"} }, "invalid repo name"},
		{"valid everything repos", func(s *Scope) { s.EverythingRepos = []string{"api"} }, ""},
		{"bad everything repo", func(s *Scope) { s.EverythingRepos = []string{".."} }, "invalid repo name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ok
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("Validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestWatches(t *testing.T) {
	base := Scope{Owner: "o", Kind: KindOrg, Enabled: true, Items: ItemsInvolving}
	tests := []struct {
		name  string
		scope Scope
		repo  string
		want  bool
	}{
		{"all repos", withRepos(base, ReposAll), "api", true},
		{"disabled scope", Scope{Enabled: false, RepoMode: ReposAll}, "api", false},
		{"include hit, case-insensitive", withRepos(base, ReposInclude, "API"), "api", true},
		{"include miss", withRepos(base, ReposInclude, "web"), "api", false},
		{"exclude hit", withRepos(base, ReposExclude, "api"), "api", false},
		{"exclude miss", withRepos(base, ReposExclude, "web"), "api", true},
		{"listed for everything outranks include", withEverything(withRepos(base, ReposInclude, "web"), "API"), "api", true},
		{"listed for everything outranks exclude", withEverything(withRepos(base, ReposExclude, "api"), "api"), "api", true},
		{"unlisted repo still follows the picks", withEverything(withRepos(base, ReposInclude, "web"), "docs"), "api", false},
		{"list ignored while items is all", withEverything(func() Scope { s := withRepos(base, ReposInclude, "web"); s.Items = ItemsAll; return s }(), "api"), "api", false},
		{"list ignored while disabled", Scope{Enabled: false, Items: ItemsInvolving, RepoMode: ReposAll, EverythingRepos: []string{"api"}}, "api", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Watches(tt.repo); got != tt.want {
				t.Errorf("Watches(%q) = %v, want %v", tt.repo, got, tt.want)
			}
		})
	}
}

func withEverything(s Scope, repos ...string) Scope {
	s.EverythingRepos = repos
	return s
}

func withRepos(s Scope, mode string, repos ...string) Scope {
	s.RepoMode, s.Repos = mode, repos
	return s
}

func TestSyncCreatesDefaultsAndKeepsChoices(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	id := Identity{Login: "willesq", Orgs: []string{"WippData", "sfmail", "not a login!"}}
	if err := st.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	scopes, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range scopes {
		got = append(got, s.Owner+":"+s.Kind+":"+map[bool]string{true: "on", false: "off"}[s.Enabled]+":"+s.Items+":"+s.RepoMode)
	}
	want := []string{"willesq:user:on:involving:all", "sfmail:org:off:involving:all", "WippData:org:off:involving:all"}
	if !slices.Equal(got, want) {
		t.Errorf("after first sync = %v, want %v", got, want)
	}

	// The user turns their own account off and an org on with picks; a later
	// discovery must not undo either.
	if _, err := st.Set(ctx, Scope{Owner: "willesq", Kind: KindUser, Items: ItemsInvolving, RepoMode: ReposAll}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(ctx, Scope{Owner: "WippData", Kind: KindOrg, Enabled: true, Items: ItemsAll,
		RepoMode: ReposInclude, Repos: []string{" web ", "api", "API", ""}, EverythingRepos: []string{" docs ", "DOCS", "site"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	me, _ := st.Get(ctx, "WILLESQ")
	org, _ := st.Get(ctx, "wippdata")
	if me.Enabled {
		t.Error("rediscovery re-enabled the user's account")
	}
	if !org.Enabled || org.Items != ItemsAll || !slices.Equal(org.Repos, []string{"api", "web"}) ||
		!slices.Equal(org.EverythingRepos, []string{"docs", "site"}) {
		t.Errorf("org scope after rediscovery = %+v", org)
	}
	if org.UpdatedAt != 1_800_000_000 {
		t.Errorf("UpdatedAt = %d", org.UpdatedAt)
	}
}

func TestSetRejectsInvalidAndGetMissing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.Set(ctx, Scope{Owner: "o", Kind: KindOrg, Items: ItemsAll, RepoMode: ReposInclude}); err == nil {
		t.Error("Set accepted include mode without repos")
	}
	if _, err := st.Get(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestStoreErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	_ = st.db.Close()
	if _, err := st.List(ctx); err == nil {
		t.Error("List on a closed db succeeded")
	}
	if _, err := st.Get(ctx, "x"); err == nil {
		t.Error("Get on a closed db succeeded")
	}
	if _, err := st.Set(ctx, Scope{Owner: "o", Kind: KindOrg, Items: ItemsAll, RepoMode: ReposAll}); err == nil {
		t.Error("Set on a closed db succeeded")
	}
	if err := st.Sync(ctx, Identity{Login: "willesq"}); err == nil {
		t.Error("Sync on a closed db succeeded")
	}
	if err := st.Sync(ctx, Identity{Orgs: []string{"o"}}); err == nil {
		t.Error("Sync of orgs on a closed db succeeded")
	}
}

// fakeGH answers the gh calls discovery and repo listing make.
type fakeGH struct {
	calls []string
	fail  string // fail any call whose args contain this
	login string
	orgs  string
	teams string
	repos map[string]string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	if f.fail != "" && strings.Contains(joined, f.fail) {
		return nil, errors.New("gh: HTTP 401")
	}
	switch {
	case slices.Contains(args, "user") && slices.Contains(args, ".login"):
		return []byte(f.login + "\n"), nil
	case slices.Contains(args, "user/orgs"):
		return []byte(f.orgs), nil
	case slices.Contains(args, "user/teams"):
		return []byte(f.teams), nil
	}
	for path, out := range f.repos {
		if slices.Contains(args, path) {
			return []byte(out), nil
		}
	}
	return nil, errors.New("unexpected gh call: " + joined)
}

func newFake() *fakeGH {
	return &fakeGH{
		login: "willesq",
		orgs:  "WippData\nsfmail\n\n",
		teams: `{"org":"WippData","slug":"allreposwrite","name":"All repos write"}` + "\n" +
			`{"org":"WippData","slug":"admins","name":"Admins"}` + "\n" +
			`{"org":"Acme","slug":"core","name":"Core"}`,
		repos: map[string]string{
			"user/repos?per_page=100&affiliation=owner": `{"name":"zeta","private":true,"archived":false}` + "\n" + `{"name":"Alpha","private":false,"archived":true}`,
			"orgs/WippData/repos?per_page=100":          `{"name":"api","private":true,"archived":false}`,
		},
	}
}

func TestIdentityIsDiscoveredSortedAndCached(t *testing.T) {
	ctx := context.Background()
	gh := newFake()
	clock := time.Unix(0, 0)
	svc := NewService(testStore(t), gh.run)
	svc.now = func() time.Time { return clock }

	id, err := svc.Identity(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if id.Login != "willesq" || !slices.Equal(id.Orgs, []string{"sfmail", "WippData"}) {
		t.Errorf("identity = %+v", id)
	}
	var teams []string
	for _, tm := range id.Teams {
		teams = append(teams, tm.Org+"/"+tm.Slug)
	}
	if !slices.Equal(teams, []string{"Acme/core", "WippData/admins", "WippData/allreposwrite"}) {
		t.Errorf("teams = %v", teams)
	}

	calls := len(gh.calls)
	if _, err := svc.Identity(ctx, false); err != nil || len(gh.calls) != calls {
		t.Errorf("cached identity called gh again (%d -> %d), err %v", calls, len(gh.calls), err)
	}
	if _, err := svc.Identity(ctx, true); err != nil || len(gh.calls) != calls {
		t.Error("a refresh within minRefresh called gh")
	}
	clock = clock.Add(minRefresh)
	if _, err := svc.Identity(ctx, true); err != nil || len(gh.calls) == calls {
		t.Error("refresh did not call gh")
	}
	calls = len(gh.calls)
	clock = clock.Add(identityTTL + time.Second)
	if _, err := svc.Identity(ctx, false); err != nil || len(gh.calls) == calls {
		t.Error("an expired cache did not call gh")
	}
}

func TestIdentityErrors(t *testing.T) {
	tests := []struct {
		name string
		gh   func(*fakeGH)
		want string
	}{
		{"login call fails", func(f *fakeGH) { f.fail = ".login" }, "401"},
		{"unusable login", func(f *fakeGH) { f.login = "" }, "no usable login"},
		{"orgs call fails", func(f *fakeGH) { f.fail = "user/orgs" }, "401"},
		{"teams call fails", func(f *fakeGH) { f.fail = "user/teams" }, "401"},
		{"teams unparseable", func(f *fakeGH) { f.teams = "{not json" }, "parse teams"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFake()
			tt.gh(gh)
			_, err := NewService(testStore(t), gh.run).Identity(context.Background(), false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestScopesSyncsAndSurvivesGitHubErrors(t *testing.T) {
	ctx := context.Background()
	gh := newFake()
	clock := time.Unix(0, 0)
	svc := NewService(testStore(t), gh.run)
	svc.now = func() time.Time { return clock }
	scopes, ghErr, err := svc.Scopes(ctx, false)
	if err != nil || ghErr != nil || len(scopes) != 3 || scopes[0].Owner != "willesq" || !scopes[0].Enabled {
		t.Fatalf("Scopes = %+v, %v, %v", scopes, ghErr, err)
	}
	gh.fail = ".login"
	clock = clock.Add(minRefresh)
	scopes, ghErr, err = svc.Scopes(ctx, true)
	if err != nil || ghErr == nil || len(scopes) != 3 {
		t.Errorf("with GitHub down: %d scopes, ghErr %v, err %v; want stored scopes and ghErr", len(scopes), ghErr, err)
	}
	_ = svc.Store.db.Close()
	if _, _, err := svc.Scopes(ctx, false); err == nil {
		t.Error("a storage failure was not reported as err")
	}
}

func TestScopesReportsSyncFailure(t *testing.T) {
	gh := newFake()
	svc := NewService(testStore(t), gh.run)
	_ = svc.Store.db.Close()
	if _, ghErr, err := svc.Scopes(context.Background(), false); err == nil || ghErr != nil {
		t.Errorf("sync on a closed db: ghErr %v, err %v", ghErr, err)
	}
}

func TestRepos(t *testing.T) {
	ctx := context.Background()
	gh := newFake()
	svc := NewService(testStore(t), gh.run)

	mine, err := svc.Repos(ctx, "WillEsq")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 || mine[0] != (Repo{Name: "Alpha", Archived: true}) || mine[1] != (Repo{Name: "zeta", Private: true}) {
		t.Errorf("own repos = %+v", mine)
	}
	org, err := svc.Repos(ctx, "WippData")
	if err != nil || len(org) != 1 || org[0].Name != "api" {
		t.Errorf("org repos = %+v, %v", org, err)
	}
	if _, err := svc.Repos(ctx, "../x"); err == nil {
		t.Error("Repos accepted an invalid login")
	}
	gh.repos["orgs/WippData/repos?per_page=100"] = "{broken"
	if _, err := svc.Repos(ctx, "WippData"); err == nil || !strings.Contains(err.Error(), "parse repos") {
		t.Errorf("unparseable repos err = %v", err)
	}
	delete(gh.repos, "orgs/WippData/repos?per_page=100")
	if _, err := svc.Repos(ctx, "sfmail"); err == nil {
		t.Error("a failed gh call was not reported")
	}
	gh2 := newFake()
	gh2.fail = ".login"
	if _, err := NewService(testStore(t), gh2.run).Repos(ctx, "WippData"); err == nil {
		t.Error("Repos without an identity succeeded")
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("  ok\x1b[31m red\u202e\n", 100); got != "ok [31m red" {
		t.Errorf("Printable = %q", got)
	}
	if got := Printable(strings.Repeat("é", 10), 5); got != "éé…" {
		t.Errorf("Printable cut = %q", got)
	}
}

func TestMemberAndCaseInsensitiveRows(t *testing.T) {
	id := Identity{Login: "willesq", Orgs: []string{"WippData"}}
	if !id.Member("WILLESQ") || !id.Member("wippdata") || id.Member("microsoft") {
		t.Error("Member mismatch")
	}
	ctx := context.Background()
	st := testStore(t)
	if err := st.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := st.Sync(ctx, Identity{Login: "WillEsq", Orgs: []string{"wippdata"}}); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.List(ctx); len(all) != 2 {
		t.Errorf("a case change created duplicate rows: %+v", all)
	}
}

func TestReposRefusesOtherOwners(t *testing.T) {
	svc := NewService(testStore(t), newFake().run)
	if _, err := svc.Repos(context.Background(), "microsoft"); !errors.Is(err, ErrNotMember) {
		t.Errorf("err = %v, want ErrNotMember", err)
	}
}

func TestIdentityCopiesAreIndependent(t *testing.T) {
	svc := NewService(testStore(t), newFake().run)
	a, _ := svc.Identity(context.Background(), false)
	a.Orgs[0] = "changed"
	b, _ := svc.Identity(context.Background(), false)
	if b.Orgs[0] == "changed" {
		t.Error("a caller changed the cached identity")
	}
}
