package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRouter routes owners in its profile's orgs through work, as a space's
// own gh profile.
type fakeRouter struct {
	work   *fakeGH
	routed map[string]bool
	err    error
	scopes []Scope
}

func (r *fakeRouter) Account(_ context.Context, owner string) (Runner, Identity, bool, error) {
	if r.err != nil {
		return nil, Identity{}, false, r.err
	}
	if !r.routed[strings.ToLower(owner)] {
		return nil, Identity{}, false, nil
	}
	return r.work.run, Identity{Login: r.work.login, Orgs: strings.Fields(r.work.orgs)}, true, nil
}

func (r *fakeRouter) ScopeOwners(context.Context) []Scope { return r.scopes }

// TestServiceRoutesProfileOnlyOrgs is SP-074.1.a1 and a3 at the service: an
// org only a space's own gh profile belongs to becomes a scope (disabled),
// counts as a membership, and its repos are listed as the profile, never as
// the default login; an error from the router refuses, with no retry.
func TestServiceRoutesProfileOnlyOrgs(t *testing.T) {
	ctx := context.Background()
	def := newFake()
	work := &fakeGH{login: "work-me", orgs: "acme\n", repos: map[string]string{
		"orgs/acme/repos?per_page=100": `{"name":"api","private":true,"archived":false}`,
	}}
	r := &fakeRouter{work: work, routed: map[string]bool{"acme": true},
		scopes: []Scope{{Owner: "acme", Kind: KindOrg}, {Owner: "work-me", Kind: KindUser}, {Owner: "../bad", Kind: KindOrg}, {Owner: "x", Kind: "team"}}}
	svc := NewService(testStore(t), def.run)
	svc.Accounts = r

	scopes, ghErr, err := svc.Scopes(ctx, false)
	if err != nil || ghErr != nil {
		t.Fatal(err, ghErr)
	}
	got := map[string]Scope{}
	for _, sc := range scopes {
		got[sc.Owner] = sc
	}
	if sc, ok := got["acme"]; !ok || sc.Enabled || sc.Kind != KindOrg {
		t.Errorf("acme scope = %+v, %v; want a disabled org scope", sc, ok)
	}
	if sc, ok := got["work-me"]; !ok || sc.Enabled || sc.Kind != KindUser {
		t.Errorf("work-me scope = %+v, %v; want a disabled user scope", sc, ok)
	}
	if _, ok := got["../bad"]; ok || len(got) != 5 {
		t.Errorf("scopes = %v", got)
	}

	if m, err := svc.Member(ctx, "acme"); err != nil || !m {
		t.Errorf("Member(acme) = %v, %v", m, err)
	}
	if m, err := svc.Member(ctx, "WippData"); err != nil || !m {
		t.Errorf("Member(WippData) = %v, %v", m, err)
	}
	repos, err := svc.Repos(ctx, "acme")
	if err != nil || len(repos) != 1 || repos[0].Name != "api" {
		t.Fatalf("acme repos = %+v, %v", repos, err)
	}
	for _, c := range def.calls {
		if strings.Contains(c, "acme") {
			t.Errorf("the default login ran %q", c)
		}
	}

	r.err = ErrNoAccess
	before := len(def.calls) + len(work.calls)
	if _, err := svc.Repos(ctx, "acme"); !errors.Is(err, ErrNoAccess) {
		t.Errorf("refused route: err = %v", err)
	}
	if _, err := svc.Member(ctx, "acme"); !errors.Is(err, ErrNoAccess) {
		t.Errorf("refused member: err = %v", err)
	}
	if after := len(def.calls) + len(work.calls); after != before {
		t.Errorf("a refused route still ran %d gh call(s)", after-before)
	}

	// A store failure while adding profile scopes is reported.
	r.err = nil
	svc2 := NewService(testStore(t), def.run)
	svc2.Accounts = &fakeRouter{scopes: []Scope{{Owner: "acme", Kind: KindOrg}}}
	_ = svc2.Store.db.Close()
	if _, _, err := svc2.Scopes(ctx, false); err == nil {
		t.Error("Scopes hid a store failure")
	}
}

func TestMarkNoAccess(t *testing.T) {
	if MarkNoAccess(nil) != nil {
		t.Error("nil became an error")
	}
	for _, msg := range []string{"gh api: HTTP 404: Not Found", "gh pr view: GraphQL: Could not resolve to a Repository", "HTTP 403: Resource not accessible by integration"} {
		err := MarkNoAccess(errors.New(msg))
		if !errors.Is(err, ErrNoAccess) || !strings.Contains(err.Error(), msg) {
			t.Errorf("MarkNoAccess(%q) = %v", msg, err)
		}
		if again := MarkNoAccess(err); again != err {
			t.Errorf("marked twice: %v", again)
		}
	}
	for _, msg := range []string{"gh: HTTP 502", "HTTP 403: API rate limit exceeded for user", "HTTP 403: You have exceeded a secondary rate limit"} {
		if err := MarkNoAccess(errors.New(msg)); errors.Is(err, ErrNoAccess) {
			t.Errorf("%q was marked as no access: %v", msg, err)
		}
	}
}

// TestCheckProfileDir is SP-074.1.a2's directory rule: a gh_config_dir must be
// an absolute, existing directory under the home, owned by the user and
// closed to everyone else.
func TestCheckProfileDir(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(parent, name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(parent, name)
		if err := os.Mkdir(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := mk(home, "gh-acme", 0o700)
	shared := mk(home, "shared", 0o777)
	underShared := mk(shared, "gh-x", 0o700)
	nested := mk(mk(home, "cfg", 0o755), "gh-y", 0o700)
	open := mk(home, "gh-open", 0o755)
	group := mk(home, "gh-group", 0o710)
	far := mk(outside, "gh-far", 0o700)
	file := filepath.Join(home, "gh-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "gh-link")
	if err := os.Symlink(far, link); err != nil {
		t.Fatal(err)
	}
	inLink := filepath.Join(home, "gh-inside")
	if err := os.Symlink(good, inLink); err != nil {
		t.Fatal(err)
	}
	if err := CheckProfileDir(good); err != nil {
		t.Errorf("owner-only directory refused: %v", err)
	}
	if err := CheckProfileDir(nested); err != nil {
		t.Errorf("a profile in a folder others can only read was refused: %v", err)
	}
	if err := CheckProfileDir(inLink); err != nil {
		t.Errorf("a link to an owner-only directory in the home refused: %v", err)
	}
	for dir, want := range map[string]string{
		"relative/gh":                  "not an absolute path",
		filepath.Join(home, "missing"): "does not exist",
		open:                           "other users can open",
		group:                          "other users can open",
		far:                            "not inside your home",
		link:                           "not inside your home",
		home:                           "not inside your home",
		file:                           "not a directory",
		underShared:                    "other users can change",
	} {
		err := CheckProfileDir(dir)
		if !errors.Is(err, ErrProfileDir) || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckProfileDir(%s) = %v, want %q", dir, err, want)
		}
	}
	t.Setenv("HOME", "")
	if err := CheckProfileDir(good); !errors.Is(err, ErrProfileDir) {
		t.Errorf("unknown home: err = %v", err)
	}
}
