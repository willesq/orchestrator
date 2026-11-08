package spaces

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var profilePolicy = Policy{Spaces: []Space{
	{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"Acme-Corp"}, GHConfigDir: "~/.config/gh-acme"},
	{Name: "globex", Kind: Isolated, GitHubOrgs: []string{"globex"}},
	{Name: "personal", Kind: Private, GitHubOrgs: []string{"me"}},
	{Name: "notes", Kind: Private},
	{Name: "oss", Kind: Public, GitHubOrgs: []string{"oss-org"}, GHConfigDir: "/profiles/oss"},
}}

func TestGHProfileFor(t *testing.T) {
	r := mustValid(t, profilePolicy, testEnv())
	for _, tc := range []struct {
		login    string
		want     GHProfile
		required bool
		ok       bool
	}{
		{login: "ACME-corp", want: GHProfile{Space: "acme", Kind: Isolated, ConfigDir: "/home/u/.config/gh-acme", Orgs: []string{"Acme-Corp"}}, required: true, ok: true},
		{login: "globex", want: GHProfile{Space: "globex", Kind: Isolated, Orgs: []string{"globex"}}, required: true, ok: true},
		{login: "me", want: GHProfile{Space: "personal", Kind: Private, Orgs: []string{"me"}}, ok: true},
		{login: "oss-org", want: GHProfile{Space: "oss", Kind: Public, ConfigDir: "/profiles/oss", Orgs: []string{"oss-org"}}, ok: true},
		{login: "unbound"},
	} {
		got, ok := r.GHProfileFor(tc.login)
		if ok != tc.ok || got.Space != tc.want.Space || got.Kind != tc.want.Kind || got.ConfigDir != tc.want.ConfigDir ||
			!slices.Equal(got.Orgs, tc.want.Orgs) || got.Required() != tc.required {
			t.Errorf("GHProfileFor(%q) = %+v, %v; want %+v, %v", tc.login, got, ok, tc.want, tc.ok)
		}
	}
	var spaces []string
	for _, p := range r.GHProfiles() {
		spaces = append(spaces, p.Space)
	}
	// notes binds no org and names no profile, so it has nothing to list.
	if !slices.Equal(spaces, []string{"acme", "globex", "oss", "personal"}) {
		t.Errorf("GHProfiles spaces = %v", spaces)
	}
	var none *Resolved
	if _, ok := none.GHProfileFor("acme-corp"); ok || none.GHProfiles() != nil {
		t.Error("no policy has GitHub profiles")
	}
}

func TestValidateGHConfigDir(t *testing.T) {
	inRepo := testEnv()
	inRepo.InGitTree = func(path string) bool { return strings.HasPrefix(path, "/home/u/code/") }
	for _, tc := range []struct {
		name string
		p    Policy
		env  Env
		rule Rule
	}{
		{
			name: "relative",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GHConfigDir: "gh-acme"}}},
			env:  testEnv(),
			rule: RuleBadBinding,
		},
		{
			name: "in a git tree",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GHConfigDir: "~/code/acme/.gh"}}},
			env:  inRepo,
			rule: RuleGHProfileInGitTree,
		},
		{
			name: "shared",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, GHConfigDir: "~/.config/gh-work"},
				{Name: "globex", Kind: Isolated, GHConfigDir: "/HOME/u/.config/gh-work/"},
			}},
			env:  testEnv(),
			rule: RuleGHProfileShared,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := mustProblems(t, tc.p, tc.env)
			if len(ps) != 1 || ps[0].Rule != tc.rule {
				t.Fatalf("problems = %+v, want one %s", ps, tc.rule)
			}
		})
	}
	big := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GHConfigDir: "/" + strings.Repeat("x/", maxDepth+1)}}}
	doc, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(doc); err == nil {
		t.Error("Parse took a gh_config_dir deeper than the limit")
	}
}

// TestOrgLookupsUseTheBindingSpacesProfile: apply looks orgs up as the gh
// profile of the space the draft binds them to, and a sync's re-check as the
// one the applied policy binds them to.
func TestOrgLookupsUseTheBindingSpacesProfile(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(nil)
	e := orgEngine(t, conn, f)
	saveAndApply(t, e, profilePolicy)
	if got := f.profiles["acme-corp"]; got.Space != "acme" || got.ConfigDir != "/home/u/.config/gh-acme" || !got.Required() {
		t.Errorf("apply looked acme-corp up as %+v", got)
	}
	if got := f.profiles["oss-org"]; got.Space != "oss" || got.ConfigDir != "/profiles/oss" {
		t.Errorf("apply looked oss-org up as %+v", got)
	}

	moved := Policy{Spaces: slices.Clone(profilePolicy.Spaces)}
	moved.Spaces[0].GHConfigDir = "/profiles/acme-2"
	saveAndApply(t, e, moved)
	f.profiles = nil
	if _, err := e.CheckOrg(ctx, "acme-corp"); err != nil {
		t.Fatal(err)
	}
	if got := f.profiles["acme-corp"]; got.ConfigDir != "/profiles/acme-2" {
		t.Errorf("sync re-check looked acme-corp up as %+v", got)
	}
}

// TestCompareShowsGHConfigDir: the operator reviewing a draft sees a changed
// gh profile, since it changes which account the space's calls run as.
func TestCompareShowsGHConfigDir(t *testing.T) {
	from := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GHConfigDir: "/profiles/acme"}}}
	to := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GHConfigDir: "/profiles/other"}}}
	got := Compare(from, to)
	if len(got) != 1 || !slices.Equal(got[0].Fields, []string{"gh_config_dir"}) ||
		!slices.Equal(got[0].Details, []string{`gh_config_dir: "/profiles/acme" -> "/profiles/other"`}) {
		t.Errorf("Compare = %+v", got)
	}
	added := Compare(Policy{}, to)
	if len(added) != 1 || !slices.Contains(added[0].Details, `gh_config_dir: "" -> "/profiles/other"`) {
		t.Errorf("an added space's details = %+v", added)
	}
}
