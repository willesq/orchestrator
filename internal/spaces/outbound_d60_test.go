package spaces

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-60 loosened SP-044's baseline. Each test below holds commands the
// strict reading refused (outbound_unchecked, or outbound_term for a
// destination gh never writes to) that are now allowed, next to commands
// that still go to another space's destination or carry its term and are
// still refused. The checker's acme term is falcon, globex's zephyr.

func wantAllowed(t *testing.T, c OutboundChecker, dir string, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if r := check(c, cmd, dir); r.Refuse {
			t.Errorf("%q: refused: %+v", cmd, r)
		}
	}
}

func wantRefused(t *testing.T, c OutboundChecker, dir string, rule GuardRule, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if r := check(c, cmd, dir); !r.Refuse || r.Rule != rule {
			t.Errorf("%q: %+v, want %s", cmd, r, rule)
		}
	}
}

// TestOutboundGHDefaultRepo is baseline rule gh_default_repo: a gh write
// without -R counts only the repo gh would use.
func TestOutboundGHDefaultRepo(t *testing.T) {
	repo := newTestRepo(t, "acme")
	repo.git("remote", "add", "gx", "https://github.com/globex/app.git")
	c := termsChecker(t)
	create := `gh pr create -t x -b "falcon"`
	// Before: origin (acme) and gx (globex) both counted, so acme's term
	// was refused. gh picks origin before gx.
	wantAllowed(t, c, repo.dir, create, `gh issue comment 1 -b "falcon"`)
	// Still refused: globex's term to acme.
	wantRefused(t, c, repo.dir, RuleOutboundTerm, `gh pr create -t x -b "zephyr"`)
	// gh repo set-default marks gx: now it is the destination.
	repo.git("config", "remote.gx.gh-resolved", "base")
	if r := check(c, create, repo.dir); !r.Refuse || r.Rule != RuleOutboundTerm || r.Dest != "globex" {
		t.Errorf("gx set as default: %+v", r)
	}
	repo.git("config", "--unset", "remote.gx.gh-resolved")
	// A mark naming another repo picks it; a mark that is no repo cannot
	// be read.
	repo.git("config", "remote.origin.gh-resolved", "globex/parent")
	if r := check(c, create, repo.dir); !r.Refuse || r.Dest != "globex" {
		t.Errorf("origin resolved to globex/parent: %+v", r)
	}
	plain := `gh pr create -t x -b x`
	repo.git("config", "remote.origin.gh-resolved", "not a repo")
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, plain)
	repo.git("config", "--unset", "remote.origin.gh-resolved")
	// gh orders upstream before origin.
	repo.git("remote", "add", "upstream", "https://github.com/globex/core.git")
	if r := check(c, create, repo.dir); !r.Refuse || r.Dest != "globex" {
		t.Errorf("upstream globex: %+v", r)
	}
	repo.git("remote", "remove", "upstream")
	// A remote gh may know though the check cannot tell (an ssh alias),
	// ordered before the first github.com remote, counts too.
	repo.git("remote", "add", "upstream", "git@work:globex/app.git")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, create)
	repo.git("remote", "remove", "upstream")
	// The hook's GH_REPO wins; GH_HOST naming another host counts every
	// remote.
	for k, v := range map[string]string{"GH_REPO": "globex/app", "GH_HOST": "ghe.example.com"} {
		env := c
		env.Getenv = func(name string) string {
			if name == k {
				return v
			}
			return ""
		}
		if r := check(env, create, repo.dir); !r.Refuse {
			t.Errorf("hook %s=%s: allowed", k, v)
		}
	}
	// What changes gh's repo in the same command cannot be read ahead.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"gh repo set-default acme/app && "+plain,
		"gh repo set-default --unset; "+plain,
		"GH_REPO= "+plain,
		"unset GH_REPO; "+plain,
		"env -u GH_REPO "+plain,
		"env -uGH_REPO "+plain,
		"env -i "+plain,
		"git config remote.gx.gh-resolved base && "+plain)
	// Outside a checkout gh's repo cannot be told.
	wantRefused(t, c, realTemp(t), RuleOutboundUnchecked, plain)
	// env -P takes a value: the command after it is still read.
	wantRefused(t, c, repo.dir, RuleOutboundTerm, `env -P /usr/bin gh pr create -t x -b "zephyr"`)
}

// TestOutboundPlainVariables is baseline rule plain_variables: $PWD,
// $USER and $HOME are read as the shell expands them.
func TestOutboundPlainVariables(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	c.Home = realTemp(t)
	t.Setenv("USER", "acme")
	wantAllowed(t, c, repo.dir,
		`git -C "$PWD" commit --allow-empty -m "falcon from $USER"`,
		`git -C ${PWD} push origin main`,
		`cd "$HOME" && cd "`+repo.dir+`" && git commit --allow-empty -m "falcon"`,
		`gh issue create -R "${USER}/app" -t x -b "falcon"`,
		`gh issue create -R $USER/app -t x -b "falcon"`)
	// Still refused: the expansion names another space's repo, or holds a
	// term, or cannot be one word.
	t.Setenv("USER", "globex")
	wantRefused(t, c, repo.dir, RuleOutboundTerm, `gh issue create -R "$USER/app" -t x -b "falcon"`)
	t.Setenv("USER", "")
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, `gh issue create -R "$USER/app" -t x -b "falcon"`)
	termDir := filepath.Join(realTemp(t), "zephyr work")
	if err := os.MkdirAll(termDir, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := newTestRepoAt(t, termDir, "acme").dir
	wantRefused(t, c, moved, RuleOutboundTerm, `git commit --allow-empty -m "built in $PWD"`)
	// Unquoted, a value with a blank is split by the shell.
	wantRefused(t, c, moved, RuleOutboundUnchecked, `git -C $PWD commit --allow-empty -m x`)
	// Any other variable is still unknown.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, `git commit --allow-empty -m "$EDITOR"`, `git commit -m "${PWD:-x}"`)
	// After a cd that cannot be followed, $PWD is unknown.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked, `cd "$(mktemp -d)" && git -C "$PWD" commit --allow-empty -m x`)
}

// TestOutboundCommandAssignments is baseline rule command_assignments: a
// literal assignment made earlier in the same command, where it always
// runs first, is read.
func TestOutboundCommandAssignments(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	gh := `gh issue create -R "$R" -t x -b "falcon"`
	wantAllowed(t, c, repo.dir,
		"R=acme/app; "+gh,
		"R=acme/app && "+gh,
		"R=acme/app\n"+gh,
		`M="ship falcon"; git commit --allow-empty -m "$M"`,
		"export R=acme/app; bash -c '"+gh+"'",
		"R=acme/app; export R; bash -c '"+gh+"'",
		`G=git; $G commit --allow-empty -m "falcon"`,
		"D="+repo.dir+"; cd \"$D\" && git commit --allow-empty -m falcon",
		"R=acme/app; R=$R; "+gh)
	// Still refused: the value names another space's repo or holds its term.
	wantRefused(t, c, repo.dir, RuleOutboundTerm,
		"R=globex/app; "+gh,
		"export R=globex/app; gh issue create -R $R -t x -b falcon",
		`M=zephyr; git commit --allow-empty -m "$M"`)
	// Where the assignment may not run first, may not last, or something
	// may have changed it since, the variable is unknown.
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		"true || R=acme/app; "+gh,
		"false && R=acme/app; "+gh,
		"if true; then R=acme/app; fi; "+gh,
		"(R=acme/app); "+gh,
		"R=acme/app | true; "+gh,
		"R=acme/app & "+gh,
		"echo $(R=acme/app); "+gh,
		"R=acme/app; read R; "+gh,
		"R=acme/app; eval R=globex/app; "+gh,
		"R=acme/app; source ./env.sh; "+gh,
		"R=acme/app; f() { R=globex/app; }; f; "+gh,
		"R=acme/app; for R in globex/app; do :; done; "+gh,
		"R=acme/app; : ${R:=globex/app}; "+gh,
		"R=acme/app; ((R=1)); "+gh,
		"R=acme/app; R+=x; "+gh,
		"R=acme/app; R=globex/app true; "+gh,
		"R=acme/app; printf -v R %s globex/app; "+gh,
		"R=acme/app; declare -n R=X; "+gh,
		"R=acme/app; declare -l R=ACME/APP; "+gh,
		"R=(acme/app); "+gh,
		"R=acme/app; bash -c '"+gh+"'",
		"R=$(cat f); "+gh,
		"R=acme/app; trap 'R=globex/app' DEBUG; "+gh)
	// GH_REPO set without export does not reach gh, which then picks its
	// own repo; the check cannot tell whether the hook's environment
	// exports it.
	gx := newTestRepo(t, "globex")
	wantRefused(t, c, gx.dir, RuleOutboundUnchecked, `GH_REPO=acme/app; gh issue create -t x -b x`)
	wantRefused(t, c, gx.dir, RuleOutboundTerm, `GH_REPO=acme/app; gh issue create -t x -b "falcon"`)
	wantAllowed(t, c, gx.dir, `export GH_REPO=acme/app; gh issue create -t x -b "falcon"`)
}

// TestOutboundGitConfigKeys is baseline rule git_config_harmless_keys:
// git -c with keys added in D-60 is read; keys that name a program, a
// destination or what is sent still fail closed.
func TestOutboundGitConfigKeys(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	wantAllowed(t, c, repo.dir,
		`git -c core.commentChar=';' commit --allow-empty -m "falcon"`,
		`git -c commit.cleanup=strip -c status.showUntrackedFiles=no commit --allow-empty -m x`,
		`git -c push.autoSetupRemote=true push`,
		`git -c color.ui=never -c gc.auto=0 -c protocol.version=2 push origin main`,
		`git -c Core.CommentChar=';' commit --allow-empty -m x`)
	wantRefused(t, c, repo.dir, RuleOutboundTerm, `git -c core.commentChar=';' commit --allow-empty -m "zephyr"`)
	wantRefused(t, c, repo.dir, RuleOutboundUnchecked,
		`git -c core.pager=cat commit --allow-empty -m x`,
		`git -c help.autocorrect=immediate psuh`,
		`git -c push.default=matching push`,
		`git -c push.followTags=true push`,
		`git -c core.commentcharx=1 commit --allow-empty -m x`,
		`git -c credential.helper=x push`,
		`git -c core.hooksPath=/tmp commit --allow-empty -m x`)
}

// TestOutboundBaselineRules: the baseline's rule ids are stable and
// unique, and every D-60 loosening is one.
func TestOutboundBaselineRules(t *testing.T) {
	want := []OutboundRule{"fail_closed_unreadable", "gh_default_repo", "plain_variables", "command_assignments",
		"git_config_harmless_keys", "pending_work_after_staging"}
	if len(OutboundBaseline) != len(want) {
		t.Fatalf("baseline has %d rules", len(OutboundBaseline))
	}
	loosens := 0
	for i, r := range OutboundBaseline {
		if r.ID != want[i] || r.Summary == "" || strings.ContainsAny(string(r.ID), " .-") {
			t.Errorf("rule %d: %+v", i, r)
		}
		if r.Loosens {
			loosens++
		}
	}
	if loosens != 4 {
		t.Errorf("%d rules loosen, want the 4 of D-60", loosens)
	}
}

// TestOutboundReasonNamesNoWord: a refusal names the action only by git's
// and gh's own words; an alias, a gh subcommand or an API method the
// session wrote, which may be a term, is left out (INV-07).
func TestOutboundReasonNamesNoWord(t *testing.T) {
	repo := newTestRepo(t, "globex")
	repo.git("config", "alias.zephyr", "push")
	c := termsChecker(t)
	for _, cmd := range []string{
		"gh zephyr do falcon",
		"gh pr zephyr -b falcon",
		"gh api -X ZEPHYR repos/globex/app/issues -f body=falcon",
		"git zephyr origin main",
		"gh extension exec zephyr",
	} {
		r := check(c, cmd, repo.dir)
		if !r.Refuse || strings.Contains(strings.ToLower(r.Reason()), "zephyr") {
			t.Errorf("%q: %+v: %q", cmd, r, r.Reason())
		}
	}
	for kind, want := range map[string]string{
		"gh pr create": "gh pr create", "gh api POST": "gh api POST", "git push": "git push",
		"gh pr zephyr": "gh pr", "gh zephyr": "a gh command", "git zephyr": "a git command",
		"a command that may send text out": "a command that may send text out",
	} {
		if got := safeKind(kind); got != want {
			t.Errorf("safeKind(%q) = %q, want %q", kind, got, want)
		}
	}
}
