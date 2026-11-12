package spaces

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func shellInput(cmd string) []byte {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

// TestOutboundActions reads the outbound actions of command lines.
func TestOutboundActions(t *testing.T) {
	cases := []struct {
		cmd     string
		kinds   []string
		unknown bool
		check   func(t *testing.T, a []Outbound)
	}{
		{cmd: "ls -la"},
		{cmd: "git status && git diff"},
		{cmd: `git commit -m "fix: thing"`, kinds: []string{"git commit"}, check: func(t *testing.T, a []Outbound) {
			if !a[0].Staged || a[0].All || a[0].Dir != "/w" {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: `git commit -am "x"`, kinds: []string{"git commit"}, check: func(t *testing.T, a []Outbound) {
			if !a[0].All {
				t.Errorf("-am: %+v", a[0])
			}
		}},
		{cmd: "git commit -F msg.txt", kinds: []string{"git commit"}, check: func(t *testing.T, a []Outbound) {
			if !slices.Equal(a[0].Files, []string{"/w/msg.txt"}) {
				t.Errorf("files %q", a[0].Files)
			}
		}},
		{cmd: "cd sub && git -C .. commit --file=/tmp/m", kinds: []string{"git commit"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Dir != "/w" || !slices.Equal(a[0].Files, []string{"/tmp/m"}) {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "git push", kinds: []string{"git push"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Remote != "" || len(a[0].Refs) != 0 || !a[0].Push {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "git push -u origin +feat:main other", kinds: []string{"git push"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Remote != "origin" || !slices.Equal(a[0].Refs, []string{"feat", "other"}) {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "git push --tags --all git@github.com:o/r.git", kinds: []string{"git push"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Remote != "git@github.com:o/r.git" || !slices.Equal(a[0].Refs, []string{"--branches"}) || !slices.Equal(a[0].Tags, []string{"*"}) {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "git tag v1", kinds: nil},
		{cmd: "git tag -a v1 -m 'release notes'", kinds: []string{"git tag"}},
		{cmd: "gh pr create --title t --body \"$(cat <<'BODY'\nlong body\nBODY\n)\"", kinds: []string{"gh pr create"}},
		{cmd: "gh pr comment 3 --body-file notes.md -R acme/app", kinds: []string{"gh pr comment"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Repo != "acme/app" || !slices.Equal(a[0].Files, []string{"/w/notes.md"}) {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "GH_REPO=globex/x gh issue create -t a -b b", kinds: []string{"gh issue create"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Repo != "globex/x" {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "gh pr list", kinds: nil},
		{cmd: "gh api repos/acme/app/issues/1/comments -f body=hi", kinds: []string{"gh api "}, check: func(t *testing.T, a []Outbound) {
			if a[0].Repo != "acme/app" {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "gh api -X PATCH /orgs/globex -F description=@d.txt", kinds: []string{"gh api PATCH"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Org != "globex" || !slices.Equal(a[0].Files, []string{"/w/d.txt"}) {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "gh api repos/acme/app/pulls", kinds: nil},
		{cmd: "gh api -X GET search/issues -f q=x", kinds: nil},
		{cmd: "gh api graphql -f query='mutation{x}'", kinds: []string{"gh api "}, check: func(t *testing.T, a []Outbound) {
			if !a[0].NoRepo {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: "gh api --input body.json repos/acme/app/issues", kinds: []string{"gh api "}},
		{cmd: "gh gist create notes.md", kinds: []string{"gh gist create"}, check: func(t *testing.T, a []Outbound) {
			if !a[0].NoRepo {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: `bash -c 'cd /x && git commit -m y'`, kinds: []string{"git commit"}, check: func(t *testing.T, a []Outbound) {
			if a[0].Dir != "/x" {
				t.Errorf("%+v", a[0])
			}
		}},
		{cmd: `env FOO=1 rtk git push origin`, kinds: []string{"git push"}},
		{cmd: `git commit -m "$MSG"`, kinds: []string{"git commit"}, unknown: true},
		{cmd: `cd "$D" && git push`, kinds: []string{"git push"}, unknown: true},
		{cmd: `eval "git push"`, unknown: true},
		{cmd: `for b in a b; do git push origin $b; done`, kinds: []string{"git push"}, unknown: true},
		{cmd: `git commit -m "unterminated`, unknown: true},
	}
	for _, c := range cases {
		acts, unknown := OutboundActions("Bash", shellInput(c.cmd), "/w")
		var kinds []string
		for _, a := range acts {
			kinds = append(kinds, a.Kind)
		}
		if len(kinds) != len(c.kinds) {
			t.Errorf("%q: kinds %q, want %q", c.cmd, kinds, c.kinds)
			continue
		}
		for i := range kinds {
			if !strings.HasPrefix(kinds[i], c.kinds[i]) {
				t.Errorf("%q: kinds %q, want %q", c.cmd, kinds, c.kinds)
			}
		}
		if unknown != c.unknown {
			t.Errorf("%q: unknown %v, want %v", c.cmd, unknown, c.unknown)
		}
		if c.check != nil && len(acts) > 0 {
			c.check(t, acts)
		}
	}
	if a, _ := OutboundActions("Read", []byte(`{"file_path":"/x/git"}`), "/w"); a != nil {
		t.Errorf("Read tool: %v", a)
	}
	if a, _ := OutboundActions("exec_command", []byte(`{"cmd":["git","commit","-m","x"]}`), "/w"); len(a) != 1 {
		t.Errorf("argv command: %v", a)
	}
}

// testRepo is a git repo whose origin is github.com/<owner>/app, with git
// kept off the user's own config.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T, owner string) testRepo {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := testRepo{t: t, dir: realTemp(t)}
	r.git("init", "-q")
	r.git("remote", "add", "origin", "https://github.com/"+owner+"/app.git")
	writeFile(t, filepath.Join(r.dir, "README.md"), "hello\n")
	r.git("add", "README.md")
	r.git("commit", "-q", "-m", "first")
	// The first commit is already on the remote.
	r.git("update-ref", "refs/remotes/origin/main", "HEAD")
	return r
}

func (r testRepo) git(args ...string) {
	r.t.Helper()
	cmd := osexec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// termsChecker is a checker for acme (the session's job, org acme) and
// globex (another job, org globex), each with its own terms.
func termsChecker(t *testing.T) OutboundChecker {
	t.Helper()
	res := mustValid(t, Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme"}},
		{Name: "globex", Kind: Isolated, GitHubOrgs: []string{"globex"}},
		{Name: "oss", Kind: Public, GitHubOrgs: []string{"oss-org"}},
	}}, testEnv())
	acme, err := ParseTerms("acme", []byte("falcon\n"))
	if err != nil {
		t.Fatal(err)
	}
	globex, err := ParseTerms("globex", []byte("zephyr\nre:GX-[0-9]+\n"))
	if err != nil {
		t.Fatal(err)
	}
	return OutboundChecker{Policy: res, Terms: []TermList{acme, globex}}
}

func check(c OutboundChecker, cmd, dir string) OutboundResult {
	return c.Check(context.Background(), "Bash", shellInput(cmd), dir)
}

// TestOutboundStagedTermCaught is SP-044.1.a2 (commit): a term in a staged
// file, not in the message, is caught for git commit.
func TestOutboundStagedTermCaught(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	writeFile(t, filepath.Join(repo.dir, "notes.md"), "ship it with the Zephyr team\n")
	repo.git("add", "notes.md")
	r := check(c, `git commit -m "add notes"`, repo.dir)
	if !r.Refuse || r.Rule != RuleOutboundTerm || r.Hit.Space != "globex" || r.Hit.Line != 1 || r.Dest != "acme" {
		t.Fatalf("staged term: %+v", r)
	}
	if strings.Contains(strings.ToLower(r.Reason()), "zephyr") {
		t.Errorf("reason %q names the term", r.Reason())
	}
	// The session's own job's term may go to its own org.
	repo.git("reset", "-q", "notes.md")
	writeFile(t, filepath.Join(repo.dir, "notes.md"), "Falcon is ours\n")
	repo.git("add", "notes.md")
	if r := check(c, `git commit -m "add notes"`, repo.dir); r.Refuse {
		t.Errorf("own term to own org: %+v", r)
	}
	// commit -a sends tracked changes that are not staged.
	repo.git("commit", "-q", "-m", "notes")
	writeFile(t, filepath.Join(repo.dir, "README.md"), "hello GX-42\n")
	if r := check(c, `git commit -am "edit"`, repo.dir); !r.Refuse || r.Hit.Space != "globex" {
		t.Errorf("commit -a: %+v", r)
	}
	// A term in the message itself, or in a message file.
	if r := check(c, `git commit --allow-empty -m "for zephyr"`, repo.dir); !r.Refuse {
		t.Errorf("message: %+v", r)
	}
	writeFile(t, filepath.Join(repo.dir, "msg.txt"), "zephyr\n")
	if r := check(c, "git commit --allow-empty -F msg.txt", repo.dir); !r.Refuse {
		t.Errorf("message file: %+v", r)
	}
}

// TestOutboundPushedTermCaught is SP-044.1.a2 (push): a term in a commit
// the push would send is caught for git push.
func TestOutboundPushedTermCaught(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	writeFile(t, filepath.Join(repo.dir, "plan.md"), "next: integrate with zephyr\n")
	repo.git("add", "plan.md")
	repo.git("commit", "-q", "-m", "plan")
	for _, cmd := range []string{"git push", "git push origin main", "git push origin HEAD:main", "git push -u origin --all"} {
		r := check(c, cmd, repo.dir)
		if !r.Refuse || r.Rule != RuleOutboundTerm || r.Hit.Space != "globex" {
			t.Errorf("%q: %+v", cmd, r)
		}
	}
	// Once that commit is on the remote, it is no longer sent.
	repo.git("update-ref", "refs/remotes/origin/main", "HEAD")
	if r := check(c, "git push origin main", repo.dir); r.Refuse {
		t.Errorf("already pushed: %+v", r)
	}
	// A push to another job's org may not carry this job's terms.
	writeFile(t, filepath.Join(repo.dir, "ours.md"), "falcon\n")
	repo.git("add", "ours.md")
	repo.git("commit", "-q", "-m", "ours")
	if r := check(c, "git push https://github.com/globex/app.git main", repo.dir); !r.Refuse || r.Hit.Space != "acme" || r.Dest != "globex" {
		t.Errorf("push to another job: %+v", r)
	}
}

// TestOutboundDestinations: gh writes go to the repo they name; a gist or
// a repo in no space holds no space, so every term is refused there.
func TestOutboundDestinations(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	cases := []struct {
		cmd    string
		refuse bool
		dest   string
	}{
		{`gh pr create -t x -b "falcon"`, false, "acme"},
		{`gh pr create -t x -b "falcon" -R globex/app`, true, "globex"},
		{`gh issue comment 1 -b "zephyr" -R globex/app`, false, "globex"},
		{`gh issue comment 1 -b "zephyr" -R oss-org/lib`, true, "oss"},
		{`gh gist create -d "falcon" f.md`, true, ""},
		{`gh api repos/someone/x/issues -f body=falcon`, true, ""},
		{`gh api graphql -f query='mutation { x(body: "zephyr") }'`, true, ""},
		{`gh api -X PATCH orgs/acme -f description="falcon"`, false, "acme"},
		{`echo falcon zephyr`, false, ""},
	}
	for _, tc := range cases {
		r := check(c, tc.cmd, repo.dir)
		if r.Refuse != tc.refuse || (tc.refuse && r.Dest != tc.dest) {
			t.Errorf("%q: %+v, want refuse %v to %q", tc.cmd, r, tc.refuse, tc.dest)
		}
	}
}

// TestOutboundFailsClosed: text the check cannot read is refused while
// another space has terms; with no terms nothing is checked.
func TestOutboundFailsClosed(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	for _, cmd := range []string{
		`git commit -m "$MSG"`,
		`cd "$D" && git push`,
		"git commit -F missing.txt",
		"gh pr comment 1 --body-file - -R acme/app",
		`gh pr create -b "$(cat notes.md)"`,
	} {
		if r := check(c, cmd, repo.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
			t.Errorf("%q: %+v", cmd, r)
		} else if r.Reason() == "" {
			t.Errorf("%q: empty reason", cmd)
		}
	}
	// An unreadable terms file of another space fails closed too.
	broken := c
	broken.TermErrs = map[string]error{"globex": os.ErrNotExist}
	if r := check(broken, `git commit -m ok`, repo.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("unreadable terms file: %+v", r)
	}
	// git failing (not a repository) fails closed.
	if r := check(c, `git commit -m ok`, t.TempDir()); !r.Refuse {
		t.Errorf("outside a repo: %+v", r)
	}
	// An unparsed command with a term in it is refused; without one, not.
	if r := check(c, `git commit -m "zephyr`, repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm {
		t.Errorf("unparsed with a term: %+v", r)
	}
	if r := check(c, `git commit -m "ok`, repo.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("unparsed without a term: %+v", r)
	}
	if r := check(OutboundChecker{}, `git commit -m "$X"`, repo.dir); r.Refuse {
		t.Errorf("no terms: %+v", r)
	}
	// A pushed ref that looks like an option is not run.
	if _, err := c.sent(context.Background(), Outbound{Kind: "git push", Push: true, Dir: repo.dir, Refs: []string{"--output=/tmp/x"}}); err == nil {
		t.Error("an option-like ref was accepted")
	}
}
