package spaces

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// githubForms are the gh and git command forms SP-043 covers, each with
// ORG standing for the account it reaches. A form whose remote comes from
// the checkout's git config runs in a checkout whose origin is ORG/app.
var githubForms = []struct {
	name, cmd string
	config    bool
}{
	{name: "gh api repos/", cmd: "gh api repos/ORG/app/pulls"},
	{name: "gh api /repos/ with a leading slash", cmd: "gh api /repos/ORG/app"},
	{name: "gh -R", cmd: "gh pr list -R ORG/app"},
	{name: "gh --repo=", cmd: "gh issue view 3 --repo=ORG/app"},
	{name: "GH_REPO", cmd: "GH_REPO=ORG/app gh pr checks 9"},
	{name: "gh repo clone", cmd: "gh repo clone ORG/app"},
	{name: "gh repo clone of a URL", cmd: "gh repo clone https://github.com/ORG/app"},
	{name: "gh repo view", cmd: "gh repo view ORG/app"},
	{name: "git clone https", cmd: "git clone https://github.com/ORG/app.git"},
	{name: "git clone ssh", cmd: "git clone git@github.com:ORG/app.git"},
	{name: "git clone ssh URL", cmd: "git clone ssh://git@github.com/ORG/app"},
	{name: "git fetch URL", cmd: "git fetch https://github.com/ORG/app main"},
	{name: "git push URL", cmd: "git push git@github.com:ORG/app.git HEAD:main"},
	{name: "git ls-remote", cmd: "git ls-remote https://github.com/ORG/app"},
	{name: "git push to a named remote", cmd: "git push origin HEAD", config: true},
	{name: "git fetch with no remote", cmd: "git fetch", config: true},
	{name: "git pull from a named remote", cmd: "git -C . pull origin main", config: true},
	{name: "gh api orgs/", cmd: "gh api orgs/ORG/repos"},
	{name: "gh api users/", cmd: "gh api users/ORG"},
	{name: "gh repo list", cmd: "gh repo list ORG --limit 100"},
	{name: "gh search --owner", cmd: "gh search repos --owner ORG secret"},
	{name: "gh search qualifier", cmd: `gh search code "org:ORG password"`},
	{name: "curl of the orgs API", cmd: "curl -s https://api.github.com/orgs/ORG/members"},
	{name: "an account's page", cmd: "open https://github.com/ORG"},
}

// TestGitHubGuardCases is SP-043.1.a1: every covered form is refused when it
// reaches another job's org (other_job_repo, in block-other-jobs and
// strict), and allowed for the session's own org and a public one.
func TestGitHubGuardCases(t *testing.T) {
	f := newGuardFixture(t)
	gitConfig := filepath.Join(f.acmeApp, ".git", "config")
	orgs := []struct {
		org     string
		refused bool
	}{{"globex", true}, {"GLOBEX", true}, {"acme", false}, {"oss-org", false}}
	n := 0
	for _, form := range githubForms {
		for _, o := range orgs {
			t.Run(form.name+"/"+o.org, func(t *testing.T) {
				if form.config {
					writeFile(t, gitConfig, "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://github.com/"+o.org+
						"/app.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[remote \"mine\"]\n\turl = git@github.com:acme/fork.git\n")
					defer os.Remove(gitConfig)
				}
				tool, in := bash(strings.ReplaceAll(form.cmd, "ORG", o.org))
				ev := f.event(tool, in, f.acmeApp)
				for _, mode := range []GuardMode{GuardBlockOtherJobs, GuardStrict} {
					d := Decide(f.pol, acmePin, GuardEnforcement{Guard: mode}, ev)
					switch {
					case o.refused && (d.Verdict != Refuse || d.Rule != RuleOtherJobRepo):
						t.Errorf("%s: %s (%s), want other_job_repo; targets %+v", mode, d.Verdict, d.Rule, ev.Targets)
					case !o.refused && mode == GuardBlockOtherJobs && d.Verdict != Allow:
						t.Errorf("%s: %s (%s), want allowed; targets %+v", mode, d.Verdict, d.Rule, ev.Targets)
					}
				}
			})
			n++
		}
	}
	if len(githubForms) < 12 {
		t.Errorf("%d forms, want at least 12", len(githubForms))
	}
	t.Logf("%d forms x %d orgs = %d cases", len(githubForms), len(orgs), n)
}

// TestGitHubTargets checks the accounts and remotes Targets reports.
func TestGitHubTargets(t *testing.T) {
	dir := realTemp(t)
	mkdirs(t, filepath.Join(dir, "repo", ".git"), filepath.Join(dir, "repo", "sub"))
	writeFile(t, filepath.Join(dir, "repo", ".git", "config"), `[remote "origin"]
	url = https://github.com/Acme/app.git
	pushurl = git@github.com:acme/app-push.git
[remote "up"]
	url = "https://github.com/upstream-org/app"
[branch "main"]
	remote = origin
`)
	// A linked worktree leads to the main checkout's config.
	wtGit := filepath.Join(dir, "repo", ".git", "worktrees", "wt")
	mkdirs(t, wtGit, filepath.Join(dir, "wt"))
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(dir, "wt", ".git"), "gitdir: "+wtGit+"\n")

	cases := []struct {
		cmd, cwd      string
		repos, orgs   []string
		noRepos       []string
		opaque        bool
		opaqueAllowed bool
	}{
		{cmd: "git push origin main", cwd: "repo/sub", repos: []string{"acme/app", "acme/app-push"}, noRepos: []string{"upstream-org/app"}},
		{cmd: "git fetch", cwd: "repo", repos: []string{"acme/app", "upstream-org/app"}},
		{cmd: "git pull up", cwd: "wt", repos: []string{"upstream-org/app"}, noRepos: []string{"acme/app"}},
		{cmd: "git push nosuch", cwd: "repo", opaque: true},
		{cmd: "git status", cwd: "repo", noRepos: []string{"acme/app"}},
		{cmd: "git fetch $REMOTE", cwd: "repo", opaque: true},
		{cmd: "git fetch origin", cwd: "."},
		{cmd: "gh api orgs/Globex", cwd: ".", orgs: []string{"globex"}},
		{cmd: "gh api orgs/$ORG", cwd: ".", opaque: true},
		{cmd: "gh repo list", cwd: ".", opaque: true},
		{cmd: "gh repo list -L 5 globex --json name", cwd: ".", orgs: []string{"globex"}, opaque: true},
		{cmd: "gh repo list $O", cwd: ".", opaque: true},
		{cmd: "gh search issues --owner=a,b x", cwd: ".", orgs: []string{"a", "b"}, opaque: true},
		{cmd: `gh search code "user:u1 repo:r1/x"`, cwd: ".", orgs: []string{"u1"}, repos: []string{"r1/x"}, opaque: true},
		{cmd: "gh search prs -R o/r x", cwd: ".", repos: []string{"o/r"}, opaque: true},
		{cmd: "gh search repos --owner bad!name", cwd: ".", opaque: true},
		{cmd: "echo https://github.com/settings/tokens https://github.com/users/u2", cwd: ".", orgs: []string{"u2"}},
		{cmd: "echo see https://github.com/acme/app/pulls", cwd: ".", repos: []string{"acme/app"}},
	}
	for _, c := range cases {
		tool, in := bash(c.cmd)
		got := extractTargets(tool, []byte(in), filepath.Join(dir, c.cwd), dir)
		for _, r := range c.repos {
			if !slices.Contains(got.GHRepos, r) {
				t.Errorf("%q: repos %q, want %q", c.cmd, got.GHRepos, r)
			}
		}
		for _, r := range c.noRepos {
			if slices.Contains(got.GHRepos, r) {
				t.Errorf("%q: repos %q, want no %q", c.cmd, got.GHRepos, r)
			}
		}
		for _, o := range c.orgs {
			if !slices.Contains(got.GHOrgs, o) {
				t.Errorf("%q: orgs %q, want %q", c.cmd, got.GHOrgs, o)
			}
		}
		if got.Opaque != c.opaque {
			t.Errorf("%q: opaque %v (%q), want %v", c.cmd, got.Opaque, got.OpaqueReasons, c.opaque)
		}
	}
	if r, ok := gitConfigRemotes(filepath.Join(dir, "nowhere")); ok || r != nil {
		t.Errorf("no checkout: %v %v", r, ok)
	}
	mkdirs(t, filepath.Join(dir, "bad"))
	writeFile(t, filepath.Join(dir, "bad", ".git"), "not a gitdir line")
	if _, _, ok, err := findGitDir(filepath.Join(dir, "bad")); ok || err == nil {
		t.Error("a .git file without gitdir was followed")
	}
}

// TestGitHubGuardMalformedOrg: an account Targets could not read is
// refused as malformed in the enforcing modes.
func TestGitHubGuardMalformedOrg(t *testing.T) {
	f := newGuardFixture(t)
	ev := GuardEvent{Kind: EventPreToolUse, Tool: "Bash", Cwd: f.acmeApp, Targets: ToolTargets{GHOrgs: []string{"a/b"}}}
	if d := Decide(f.pol, acmePin, GuardEnforcement{}, ev); d.Verdict != Refuse || d.Rule != RuleMalformedTarget {
		t.Errorf("malformed account: %s (%s)", d.Verdict, d.Rule)
	}
	ev.Targets.GHOrgs, ev.Cwd = []string{"willesq"}, f.globexApp
	if d := Decide(f.pol, GuardPin{Found: true, Space: "globex"}, GuardEnforcement{Guard: GuardStrict}, ev); d.Verdict != Refuse || d.Rule != RuleUnreadableSpace {
		t.Errorf("a private space's account in strict: %s (%s)", d.Verdict, d.Rule)
	}
}
