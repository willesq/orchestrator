package spaces

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutboundReviewCases are the SP-044 security review's reproducers:
// each sends acme's term to globex (or another job's term to acme) in a
// form the first version let through, and is now refused.
func TestOutboundReviewCases(t *testing.T) {
	repo := newTestRepo(t, "acme")
	repo.git("remote", "add", "gx", "https://github.com/globex/app.git")
	writeFile(t, filepath.Join(repo.dir, "plan.md"), "integrate falcon\n")
	repo.git("add", "plan.md")
	repo.git("commit", "-q", "-m", "plan")
	other := realTemp(t)
	c := termsChecker(t)
	refused := []string{
		// A term right after a newline or tab escape in the JSON input.
		"git commit --allow-empty -m \"$(cat <<'EOF'\nfix\nZephyr rollout\nEOF\n)\"",
		"git commit --allow-empty -m \"fix\n\tzephyr\"",
		// Forms that hid the command.
		"g'i't push gx main",
		"timeout 60 git push gx main",
		"nice -n 10 git push gx main",
		"sudo -u me git push gx main",
		"bash -lc 'git push gx main'",
		"G=git; $G push gx main",
		"eval git push gx main",
		"echo main | xargs git push gx",
		// Another repository or config than the check reads.
		"pushd " + other + " && git push gx main",
		"git --git-dir=" + other + "/.git push gx main",
		"GIT_DIR=" + other + "/.git git push gx main",
		"export GIT_DIR=" + other + "/.git; git push gx main",
		"env -C " + other + " git push gx main",
		"git -c alias.p=push p gx main",
		"git -c url.https://github.com/globex/.pushInsteadOf=x: push x:app main",
		// Destinations named outside -R.
		"gh pr comment https://github.com/globex/app/pull/1 -b falcon",
		"export GH_REPO=globex/app; gh issue create -t a -b falcon",
		"gh repo create globex/new --public -d falcon",
		// gh writes the first version did not list.
		"gh variable set X --body falcon -R globex/app",
		"gh workflow run w -f x=falcon -R globex/app",
		"gh issue reopen 1 --comment falcon -R globex/app",
		"gh api https://api.github.com/repos/globex/app/issues -f body=falcon",
		"gh someext do falcon",
		// git commands that send in ways the check does not read.
		"git send-email --to=x@globex.example HEAD~1",
		"git push --recurse-submodules=on-demand gx main",
	}
	for _, cmd := range refused {
		if r := check(c, cmd, repo.dir); !r.Refuse {
			t.Errorf("%q: allowed: %+v", cmd, r)
		}
	}
	// A gist's files are read.
	writeFile(t, filepath.Join(repo.dir, "note.md"), "falcon\n")
	if r := check(c, "gh gist create note.md", repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm {
		t.Errorf("gist file: %+v", r)
	}
	// An annotated tag's message is pushed with the tag.
	repo.git("update-ref", "refs/remotes/gx/main", "HEAD")
	repo.git("tag", "-a", "v9", "-m", "falcon release")
	for _, cmd := range []string{"git push gx v9", "git push gx --tags", "git push --follow-tags gx main"} {
		if r := check(c, cmd, repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm {
			t.Errorf("%q: %+v", cmd, r)
		}
	}
	// A configured push refspec pushes more than HEAD.
	repo.git("checkout", "-q", "-b", "side")
	repo.git("commit", "-q", "--allow-empty", "-m", "side")
	repo.git("checkout", "-q", "main")
	repo.git("branch", "-q", "-f", "other-branch", "side")
	writeFile(t, filepath.Join(repo.dir, "b.md"), "falcon again\n")
	repo.git("checkout", "-q", "other-branch")
	repo.git("add", "b.md")
	repo.git("commit", "-q", "-m", "b")
	repo.git("checkout", "-q", "main")
	repo.git("config", "remote.gx.push", "refs/heads/*:refs/heads/*")
	if r := check(c, "git push gx", repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm {
		t.Errorf("configured push refspec: %+v", r)
	}
	// gh without -R writes to the repo gh picks (D-60, gh_default_repo):
	// here origin (acme) before gx (globex), so acme's term may go, and
	// once gh repo set-default marks gx it is refused.
	if r := check(c, `gh pr create -t x -b "falcon"`, repo.dir); r.Refuse {
		t.Errorf("gh with two remotes, origin first: %+v", r)
	}
	repo.git("config", "remote.gx.gh-resolved", "base")
	if r := check(c, `gh pr create -t x -b "falcon"`, repo.dir); !r.Refuse || r.Dest != "globex" {
		t.Errorf("gh with gx set as default: %+v", r)
	}
	repo.git("config", "--unset", "remote.gx.gh-resolved")
	// So does the hook's own GH_REPO.
	gh := c
	gh.Getenv = func(k string) string {
		if k == "GH_REPO" {
			return "globex/app"
		}
		return ""
	}
	one := newTestRepo(t, "acme")
	if r := check(gh, `gh pr create -t x -b "falcon"`, one.dir); !r.Refuse {
		t.Errorf("hook GH_REPO: %+v", r)
	}
	// An alias defined in the checkout is refused; an unknown subcommand
	// that is no alias is not an outbound action.
	one.git("config", "alias.ship", "push gx main")
	if r := check(c, "git ship", one.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("alias: %+v", r)
	}
	if r := check(c, "git frobnicate", one.dir); r.Refuse {
		t.Errorf("unknown subcommand: %+v", r)
	}
	// cd ~ is followed.
	home := filepath.Dir(one.dir)
	ch := c
	ch.Home = home
	if r := check(ch, "cd ~/"+filepath.Base(one.dir)+` && git commit --allow-empty -m "ok"`, "/"); r.Refuse {
		t.Errorf("cd ~: %+v", r)
	}
}

// TestOutboundNoFilterRun: commit -a reads tracked changes with the
// checkout's clean filters switched off, so a repository cannot make the
// hook run a program.
func TestOutboundNoFilterRun(t *testing.T) {
	repo := newTestRepo(t, "acme")
	marker := filepath.Join(t.TempDir(), "ran")
	writeFile(t, filepath.Join(repo.dir, ".gitattributes"), "*.md filter=x\n")
	repo.git("add", ".gitattributes")
	repo.git("commit", "-q", "-m", "attrs")
	repo.git("config", "filter.x.clean", "touch "+marker+"; cat")
	writeFile(t, filepath.Join(repo.dir, "README.md"), "changed zephyr\n")
	c := termsChecker(t)
	if r := check(c, `git commit -am "x"`, repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm {
		t.Errorf("commit -a: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the checkout's clean filter ran")
	}
}

// TestRedactJSONAndTargets: redaction decodes JSON, so a term after an
// escape is found, keeps & as it is, and covers targets.
func TestRedactJSONAndTargets(t *testing.T) {
	l, err := ParseTerms("x", []byte("zephyr\nAT&T\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(RedactJSON([]byte(`{"content":"a\nZephyr b\tzephyr c AT&T"}`), []TermList{l}))
	var v map[string]string
	if err := json.Unmarshal([]byte(got), &v); err != nil || strings.Contains(strings.ToLower(got), "zephyr") || strings.Contains(got, "AT&T") {
		t.Errorf("RedactJSON = %s (%v)", got, err)
	}
	if got := string(RedactJSON([]byte("not json zephyr"), []TermList{l})); got != "not json [term]" {
		t.Errorf("not JSON: %q", got)
	}
	tg := RedactTargets(ToolTargets{Paths: []string{"/w/zephyr/x"}, GHRepos: []string{"zephyr/app"}}, []TermList{l})
	if tg.Paths[0] != "/w/[term]/x" || tg.GHRepos[0] != "[term]/app" {
		t.Errorf("targets %+v", tg)
	}
	if !strings.Contains(JSONText([]byte(`{"a":["x\ny",{"k":"z"}]}`)), "x\ny") || JSONText([]byte("raw")) != "raw" {
		t.Error("JSONText")
	}
	if _, err := ParseTerms("x", []byte("re:(?P<terma>q)\n")); err == nil {
		t.Error("a reserved group name was accepted")
	}
	if p := TermsPaths(mustValid(t, Policy{Spaces: []Space{{Name: "a", Kind: Isolated, TermsFile: "~"}}}, Env{Home: "/h", InGitTree: func(string) bool { return false }}), "/h"); p["a"] != "/h" {
		t.Errorf("TermsPaths %v", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunGit(ctx, "/", "version"); err == nil {
		t.Error("RunGit ran with a cancelled context")
	}
	if isGitHubHost("github.evil.example") || !isGitHubHost("github-acme") || !isGitHubHost("ssh.github.com") {
		t.Error("isGitHubHost")
	}
}
