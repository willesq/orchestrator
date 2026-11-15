package spaces

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func (r testRepo) out(args ...string) string {
	r.t.Helper()
	out, err := osexec.Command("git", append([]string{"-C", r.dir}, args...)...).Output()
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

const zeroSHA = "0000000000000000000000000000000000000000"

func hookCheck(c OutboundChecker, in GitHookInput) OutboundResult {
	return c.CheckGitHook(context.Background(), in)
}

// TestGitHookCommitCheck: commit-msg reads the message file and the
// staged changes, and refuses another space's term in either.
func TestGitHookCommitCheck(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	in := GitHookInput{Hook: GitHookCommitMsg, Dir: repo.dir, MsgFile: msg}

	writeFile(t, msg, "talk to Zephyr\n")
	if r := hookCheck(c, in); !r.Refuse || r.Rule != RuleOutboundTerm || r.Hit.Space != "globex" || r.Dest != "acme" || r.Kind != "git commit" {
		t.Errorf("term in message: %+v", r)
	}
	writeFile(t, msg, "about falcon\n")
	if r := hookCheck(c, in); r.Refuse {
		t.Errorf("acme's own term to acme: %+v", r)
	}
	writeFile(t, msg, "add notes\n")
	writeFile(t, filepath.Join(repo.dir, "notes.md"), "GX-12 is next\n")
	repo.git("add", "notes.md")
	if r := hookCheck(c, in); !r.Refuse || r.Hit.Space != "globex" {
		t.Errorf("term in staged file: %+v", r)
	}
	repo.git("reset", "-q")
	if r := hookCheck(c, in); r.Refuse {
		t.Errorf("clean commit: %+v", r)
	}

	if r := hookCheck(c, GitHookInput{Hook: GitHookCommitMsg, Dir: repo.dir}); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("no message file: %+v", r)
	}
	if r := hookCheck(c, GitHookInput{Hook: GitHookCommitMsg, Dir: repo.dir, MsgFile: msg + ".gone"}); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("unreadable message file: %+v", r)
	}
	if r := hookCheck(c, GitHookInput{Hook: "post-commit", Dir: repo.dir}); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("unknown hook: %+v", r)
	}
	if r := hookCheck(OutboundChecker{Policy: c.Policy}, GitHookInput{Hook: "post-commit"}); r.Refuse {
		t.Errorf("no terms anywhere: %+v", r)
	}
}

// TestGitHookPushCheck: pre-push reads the commits and tags its updates
// send, less what the remote holds.
func TestGitHookPushCheck(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	first := repo.out("rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo.dir, "plan.md"), "integrate with zephyr\n")
	repo.git("add", "plan.md")
	repo.git("commit", "-q", "-m", "plan")
	head := repo.out("rev-parse", "HEAD")
	main := PushUpdate{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: first}
	push := func(remote, url string, ups ...PushUpdate) OutboundResult {
		return hookCheck(c, GitHookInput{Hook: GitHookPrePush, Dir: repo.dir, Remote: remote, URL: url, Updates: ups})
	}
	const acmeURL = "https://github.com/acme/app.git"

	if r := push("origin", acmeURL, main); !r.Refuse || r.Rule != RuleOutboundTerm || r.Hit.Space != "globex" || r.Kind != "git push" {
		t.Errorf("pushed term: %+v", r)
	}
	// A new branch: the remote holds nothing there, its tracking refs still count.
	if r := push("origin", acmeURL, PushUpdate{"refs/heads/main", head, "refs/heads/new", zeroSHA}); !r.Refuse {
		t.Errorf("new branch: %+v", r)
	}
	// The remote already holds the commit, though no tracking ref says so.
	held := main
	held.RemoteSHA = head
	if r := push("origin", acmeURL, held); r.Refuse {
		t.Errorf("already held: %+v", r)
	}
	// A remote object this checkout lacks excludes nothing.
	unknown := main
	unknown.RemoteSHA = strings.Repeat("ab", 20)
	if r := push("origin", acmeURL, unknown); !r.Refuse {
		t.Errorf("unknown remote object: %+v", r)
	}
	// A deletion sends nothing; nor does no update at all.
	if r := push("origin", acmeURL, PushUpdate{"(delete)", zeroSHA, "refs/heads/old", first}); r.Refuse {
		t.Errorf("deletion: %+v", r)
	}
	if r := push("origin", acmeURL); r.Refuse {
		t.Errorf("no updates: %+v", r)
	}
	// A push to a URL goes where the URL says: acme's term may not go to globex.
	repo.git("reset", "-q", "--hard", first)
	writeFile(t, filepath.Join(repo.dir, "ours.md"), "falcon\n")
	repo.git("add", "ours.md")
	repo.git("commit", "-q", "-m", "ours")
	ours := PushUpdate{"refs/heads/main", repo.out("rev-parse", "HEAD"), "refs/heads/main", zeroSHA}
	if r := push("origin", acmeURL, ours); r.Refuse {
		t.Errorf("acme's term to acme: %+v", r)
	}
	globexURL := "https://github.com/globex/app.git"
	if r := push(globexURL, globexURL, ours); !r.Refuse || r.Hit.Space != "acme" || r.Dest != "globex" {
		t.Errorf("acme's term to globex by URL: %+v", r)
	}
	// insteadOf: git passes the URL as typed and the rewritten one; the second counts.
	if r := push("gx:app", globexURL, ours); !r.Refuse || r.Dest != "globex" {
		t.Errorf("rewritten URL: %+v", r)
	}
	// A bare word that names no remote is a directory, which holds no space.
	if r := push("backup", "backup", ours); !r.Refuse || r.Dest != "" {
		t.Errorf("directory push: %+v", r)
	}
	if r := push("-x", "-x", ours); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("option-like remote: %+v", r)
	}
	if r := push("", "", ours); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("no remote: %+v", r)
	}
	// A remote object that is not an object name is never passed to git.
	bad := ours
	bad.RemoteSHA = "HEAD"
	if r := push("origin", acmeURL, bad); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("remote object HEAD: %+v", r)
	}

	// A tag's message is sent with it.
	repo.git("reset", "-q", "--hard", first)
	repo.git("tag", "-a", "v1", "-m", "release for zephyr")
	tag := repo.out("rev-parse", "v1")
	if r := push("origin", acmeURL, PushUpdate{"refs/tags/v1", tag, "refs/tags/v1", zeroSHA}); !r.Refuse || r.Hit.Space != "globex" {
		t.Errorf("tag message: %+v", r)
	}
}

func TestParsePushUpdates(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 64)
	ups, err := ParsePushUpdates(strings.NewReader("refs/heads/x " + a + " refs/heads/y " + zeroSHA + "\r\n\n(delete) " + zeroSHA + " refs/heads/z " + b + "\n"))
	if err != nil || len(ups) != 2 || ups[0].LocalSHA != a || ups[0].RemoteRef != "refs/heads/y" || ups[1].RemoteSHA != b {
		t.Fatalf("%+v %v", ups, err)
	}
	for _, in := range []string{"refs/heads/x " + a + " refs/heads/y", "refs/heads/x HEAD refs/heads/y " + a,
		"r " + a + " r " + strings.Repeat("g", 40), "r " + a + " r " + a + " extra"} {
		if _, err := ParsePushUpdates(strings.NewReader(in)); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	many := strings.Repeat("r "+a+" r "+a+"\n", maxPushUpdates+1)
	if _, err := ParsePushUpdates(strings.NewReader(many)); err == nil {
		t.Error("too many updates accepted")
	}
	if _, err := ParsePushUpdates(strings.NewReader(strings.Repeat("x", 70<<10))); err == nil {
		t.Error("overlong line accepted")
	}
}

// TestGuardFilesReadTerms: the hooks read the terms switch and policy from
// the snapshot, and cannot know them when it is unreadable.
func TestGuardFilesReadTerms(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := GuardFiles{Dir: filepath.Join(dir, "guard"), DB: filepath.Join(dir, "orch.db")}
	if g := f.ReadTerms(ctx); !g.Known || g.Policy != nil {
		t.Errorf("nothing applied: %+v", g)
	}
	terms := filepath.Join(dir, "globex.txt")
	writeFile(t, terms, "zephyr\n")
	p := Policy{Spaces: []Space{{Name: "globex", Kind: Isolated, TermsFile: terms}}}
	snap := GuardSnapshot{Revision: 2, Policy: &p, Home: "/Users/u", Terms: GuardSwitch{State: SwitchOff}}
	if err := WriteGuardSnapshot(f.Dir, snap); err != nil {
		t.Fatal(err)
	}
	if g := f.ReadTerms(ctx); !g.Known || g.Policy == nil || g.State != SwitchOff || g.Home != "/Users/u" {
		t.Errorf("applied: %+v", g)
	}
	snap.Broken = true
	if err := WriteGuardSnapshot(f.Dir, snap); err != nil {
		t.Fatal(err)
	}
	if g := f.ReadTerms(ctx); g.Known || g.State != SwitchOff {
		t.Errorf("broken: %+v", g)
	}
	bad := Policy{Spaces: []Space{{Name: "x", Kind: Isolated}, {Name: "x", Kind: Isolated}}}
	if err := WriteGuardSnapshot(f.Dir, GuardSnapshot{Revision: 3, Policy: &bad}); err != nil {
		t.Fatal(err)
	}
	if g := f.ReadTerms(ctx); g.Known {
		t.Errorf("invalid policy: %+v", g)
	}
	if err := os.WriteFile(GuardSnapshotPath(f.Dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g := f.ReadTerms(ctx); g.Known {
		t.Errorf("unreadable: %+v", g)
	}
}

// TestCheckGitHookTerms: the check reads the terms files the policy names,
// allows with the layer off and refuses when the policy is not known.
func TestCheckGitHookTerms(t *testing.T) {
	repo := newTestRepo(t, "acme")
	ctx := context.Background()
	terms := filepath.Join(t.TempDir(), "globex.txt")
	writeFile(t, terms, "zephyr\n")
	res := mustValid(t, Policy{Spaces: []Space{{Name: "globex", Kind: Isolated, TermsFile: terms}}}, testEnv())
	msg := filepath.Join(t.TempDir(), "msg")
	writeFile(t, msg, "zephyr\n")
	in := GitHookInput{Hook: GitHookCommitMsg, Dir: repo.dir, MsgFile: msg}
	if r := CheckGitHook(ctx, GuardTerms{Known: true, Policy: res}, in); !r.Refuse || r.Hit.Space != "globex" {
		t.Errorf("on: %+v", r)
	}
	if r := CheckGitHook(ctx, GuardTerms{Known: true, Policy: res, State: SwitchOff}, in); r.Refuse {
		t.Errorf("off: %+v", r)
	}
	in.Hook = GitHookPrePush
	if r := CheckGitHook(ctx, GuardTerms{}, in); !r.Refuse || r.Rule != RuleOutboundUnchecked || r.Kind != "git push" {
		t.Errorf("unknown policy: %+v", r)
	}
	if r := CheckGitHook(ctx, GuardTerms{Known: true}, in); r.Refuse {
		t.Errorf("no policy: %+v", r)
	}
}

// TestGitHookPushReview holds the SP-045 security review's cases: the
// destination is git's URL, not the remote's config; tracking refs count
// only for remotes fetched from the destination's spaces; ref names and
// tag objects pushed by name are text sent.
func TestGitHookPushReview(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	first := repo.out("rev-parse", "HEAD")
	push := func(remote, url string, ups ...PushUpdate) OutboundResult {
		return hookCheck(c, GitHookInput{Hook: GitHookPrePush, Dir: repo.dir, Remote: remote, URL: url, Updates: ups})
	}
	const acmeURL, globexURL = "https://github.com/acme/app.git", "https://github.com/globex/app.git"

	// 1. origin is acme's, but git -c remote.origin.pushurl=<globex> sends to
	// globex: the URL git passes decides, so acme's term is refused there.
	writeFile(t, filepath.Join(repo.dir, "ours.md"), "falcon\n")
	repo.git("add", "ours.md")
	repo.git("commit", "-q", "-m", "ours")
	head := repo.out("rev-parse", "HEAD")
	up := PushUpdate{"refs/heads/main", head, "refs/heads/main", first}
	if r := push("origin", globexURL, up); !r.Refuse || r.Hit.Space != "acme" || r.Dest != "globex" {
		t.Errorf("override of origin's push URL: %+v", r)
	}

	// 2. Tracking refs fetched from acme do not count for a push to globex:
	// the commit is on origin/main, yet it is still sent to globex.
	repo.git("update-ref", "refs/remotes/origin/main", head)
	newBranch := PushUpdate{"refs/heads/main", head, "refs/heads/main", zeroSHA}
	if r := push("origin", globexURL, newBranch); !r.Refuse || r.Hit.Space != "acme" {
		t.Errorf("acme tracking refs trusted for globex: %+v", r)
	}
	if r := push("origin", acmeURL, newBranch); r.Refuse {
		t.Errorf("acme tracking refs for acme: %+v", r)
	}
	repo.git("remote", "add", "gx", globexURL)
	repo.git("update-ref", "refs/remotes/gx/main", head)
	if r := push("gx", globexURL, newBranch); r.Refuse {
		t.Errorf("globex's own tracking refs: %+v", r)
	}
	repo.git("update-ref", "-d", "refs/remotes/gx/main")
	repo.git("update-ref", "refs/remotes/origin/main", first)

	// 3. A ref name with a term is refused, though no commit carries one.
	repo.git("reset", "-q", "--hard", first)
	if r := push("origin", acmeURL, PushUpdate{"refs/heads/main", first, "refs/heads/zephyr-work", zeroSHA}); !r.Refuse || r.Hit.Space != "globex" {
		t.Errorf("ref name with a term: %+v", r)
	}

	// 4. An annotated tag pushed by its object to a branch, and a tag of a tag.
	repo.git("tag", "-a", "t1", "-m", "notes on zephyr")
	repo.git("tag", "-a", "t2", "-m", "plain", "t1")
	for _, name := range []string{"t1", "t2"} {
		obj := repo.out("rev-parse", name)
		if r := push("origin", acmeURL, PushUpdate{obj, obj, "refs/heads/rel", zeroSHA}); !r.Refuse || r.Hit.Space != "globex" {
			t.Errorf("tag %s pushed by object: %+v", name, r)
		}
	}
}

func TestGitHookTagObjects(t *testing.T) {
	sha := strings.Repeat("a", 40)
	fake := func(kinds map[string]string, bodies map[string]string) OutboundChecker {
		return OutboundChecker{Git: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			obj := args[len(args)-1]
			if args[1] == "-t" {
				return []byte(kinds[obj] + "\n"), nil
			}
			return []byte(bodies[obj]), nil
		}}
	}
	// A tag that names no object, and tags nested past the bound.
	if _, err := fake(map[string]string{sha: "tag"}, map[string]string{sha: "type commit\n"}).tagObjects(context.Background(), "/d", []string{sha}); err == nil {
		t.Error("tag naming no object accepted")
	}
	if _, err := fake(map[string]string{sha: "tag"}, map[string]string{sha: "object " + sha + "\n"}).tagObjects(context.Background(), "/d", []string{sha}); err == nil {
		t.Error("endless tag chain accepted")
	}
	failing := OutboundChecker{Git: func(context.Context, string, ...string) ([]byte, error) { return nil, os.ErrNotExist }}
	if _, err := failing.tagObjects(context.Background(), "/d", []string{sha}); err == nil {
		t.Error("git failure accepted")
	}
	if r := hookCheck(OutboundChecker{Policy: termsChecker(t).Policy, Terms: termsChecker(t).Terms, Git: failing.Git},
		GitHookInput{Hook: GitHookPrePush, Dir: "/d", Remote: "o", URL: "https://github.com/acme/app",
			Updates: []PushUpdate{{"refs/heads/x", sha, "refs/heads/x", zeroSHA}}}); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("git failing: %+v", r)
	}
}

// TestGitHookPushNoSpaceRemotes: for a destination in no space, a remote
// fetched from another place in no space vouches for nothing; one fetched
// from the destination's own URL does (SP-045 re-review).
func TestGitHookPushNoSpaceRemotes(t *testing.T) {
	repo := newTestRepo(t, "acme")
	c := termsChecker(t)
	writeFile(t, filepath.Join(repo.dir, "w.md"), "falcon\n")
	repo.git("add", "w.md")
	repo.git("commit", "-q", "-m", "w")
	head := repo.out("rev-parse", "HEAD")
	repo.git("remote", "add", "work", "https://git.example.com/acme/app.git")
	repo.git("update-ref", "refs/remotes/work/main", head)
	const dest = "https://gitlab.com/me/x.git"
	up := PushUpdate{"refs/heads/main", head, "refs/heads/main", zeroSHA}
	in := GitHookInput{Hook: GitHookPrePush, Dir: repo.dir, Remote: dest, URL: dest, Updates: []PushUpdate{up}}
	if r := hookCheck(c, in); !r.Refuse || r.Hit.Space != "acme" || r.Dest != "" {
		t.Errorf("another no-space remote trusted: %+v", r)
	}
	repo.git("remote", "add", "mine", dest)
	repo.git("update-ref", "refs/remotes/mine/main", head)
	in.Remote = "mine"
	if r := hookCheck(c, in); r.Refuse {
		t.Errorf("the destination's own remote: %+v", r)
	}
}

// TestGitHookOutboundRules (SP-075): a git hook applies the operator's
// destination rules, which only make it stricter, but not the baseline's
// switches, which are about reading shell; rules that cannot be read are
// refused.
func TestGitHookOutboundRules(t *testing.T) {
	repo := newTestRepo(t, "acme")
	base := termsChecker(t)
	writeFile(t, filepath.Join(repo.dir, "w.md"), "falcon\n")
	repo.git("add", "w.md")
	repo.git("commit", "-q", "-m", "w")
	head := repo.out("rev-parse", "HEAD")
	const sdk = "https://github.com/acme/public-sdk.git"
	repo.git("remote", "add", "sdk", sdk)
	up := PushUpdate{"refs/heads/main", head, "refs/heads/main", zeroSHA}
	in := GitHookInput{Hook: GitHookPrePush, Dir: repo.dir, Remote: sdk, URL: sdk, Updates: []PushUpdate{up}}
	// The baseline lets acme's term go to acme's repo.
	if r := hookCheck(base, in); r.Refuse {
		t.Fatalf("baseline: %+v", r)
	}
	rs, _, err := OutboundRules{}.Add("destination", "acme/public-sdk")
	if err != nil {
		t.Fatal(err)
	}
	rs, _, _ = rs.Switch(string(OutboundFailClosed), false)
	c := base
	c.Rules = &rs
	if r := hookCheck(c, in); !r.Refuse || r.Rule != RuleOutboundTerm || r.Hit.Space != "acme" || r.Dest != "" {
		t.Errorf("destination rule: %+v", r)
	}
	// Commits already on origin (acme) are not trusted for the repo the
	// rule covers, so they are sent and checked.
	repo.git("update-ref", "refs/remotes/origin/main", head)
	if r := hookCheck(c, in); !r.Refuse {
		t.Errorf("destination rule with origin's tracking ref: %+v", r)
	}
	// fail_closed_unreadable off does not let a missing message file
	// through in a hook.
	if r := hookCheck(c, GitHookInput{Hook: GitHookCommitMsg, Dir: repo.dir, MsgFile: "/nonexistent"}); !r.Refuse ||
		r.Rule != RuleOutboundUnchecked {
		t.Errorf("baseline switch applied in a hook: %+v", r)
	}
	broken := base
	broken.RulesBroken = true
	if r := hookCheck(broken, in); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("broken rules: %+v", r)
	}
	bad := base
	bad.Rules = &OutboundRules{Off: []OutboundRule{"nope"}}
	if r := hookCheck(bad, in); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("invalid rules: %+v", r)
	}
}
