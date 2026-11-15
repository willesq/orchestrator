package cli

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// gitHookCLIEnv makes this test binary run as orch: the hooks the tests
// install name it as their orch, and git runs them with this set.
const gitHookCLIEnv = "ORCH_TEST_GITHOOK_CLI"

// Reason: init runs before TestMain, so the hook's orch never runs tests.
func init() {
	if os.Getenv(gitHookCLIEnv) != "1" {
		return
	}
	root := NewRootCmd(nil)
	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// gitHookSnapshot writes the guard's snapshot in a fresh data directory:
// acme holds the term falcon and globex zephyr; terms is the terms
// layer's switch.
func gitHookSnapshot(t *testing.T, terms string) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	dir := t.TempDir()
	for name, body := range map[string]string{"acme.txt": "falcon\n", "globex.txt": "zephyr\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "acme", Kind: spaces.Isolated, TermsFile: filepath.Join(dir, "acme.txt")},
		{Name: "globex", Kind: spaces.Isolated, TermsFile: filepath.Join(dir, "globex.txt")},
	}}
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 3, Policy: &p, Home: "/Users/user",
		Protection: spaces.Protection{Write: []string{paths.GuardDir()}}, Terms: spaces.GuardSwitch{State: terms}}); err != nil {
		t.Fatal(err)
	}
}

// gitIn runs git in dir and returns its combined output.
func gitIn(dir string, args ...string) (string, error) {
	cmd := osexec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x",
		"-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitIn(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// gitHookRepo is a checkout of acme/app (an org in no space, so every
// space's terms are refused there) whose pushes go to a local bare
// repository, with orch's hooks installed over a commit-msg and a
// pre-push hook of the user's that record how they were run in marks.
func gitHookRepo(t *testing.T) (dir, marks string) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(gitHookCLIEnv, "1")
	// The install's backups and hook registry stay out of the real data
	// dir; a test's own snapshot directory is kept.
	if os.Getenv("ORCH_DATA_DIR") == "" {
		t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	}
	dir, _ = filepath.EvalSymlinks(t.TempDir())
	bare := filepath.Join(t.TempDir(), "remote.git")
	marks = filepath.Join(t.TempDir(), "marks")
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, t.TempDir(), "init", "-q", "--bare", bare)
	mustGit(t, dir, "remote", "add", "origin", "https://github.com/acme/app.git")
	mustGit(t, dir, "remote", "set-url", "--push", "origin", bare)
	hooks := filepath.Join(dir, ".git", "hooks")
	user := map[string]string{
		"commit-msg": "#!/bin/sh\necho \"commit-msg $1\" >> " + marks + "\n",
		"pre-push":   "#!/bin/sh\necho \"pre-push $1 $2\" >> " + marks + "\ncat >> " + marks + "\n",
	}
	for name, body := range user {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(factory(&fakeClient{}, nil), "", "guard", "git-hook", "--install", "--apply", "--repo", dir, "--orch", exe)
	if err != nil || !strings.Contains(out, "2 hook(s) changed") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	return dir, marks
}

func readMarks(t *testing.T, marks string) string {
	t.Helper()
	b, err := os.ReadFile(marks)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(marks)
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// refusedFor checks a git command's output is orch's refusal naming the
// term's space and never the term.
func refusedFor(t *testing.T, what, out string, err error, space string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was not refused:\n%s", what, out)
	}
	if !strings.Contains(out, "rule outbound_term") || !strings.Contains(out, fmt.Sprintf("space %q", space)) {
		t.Fatalf("%s refusal:\n%s", what, out)
	}
	for _, term := range []string{"falcon", "zephyr"} {
		if strings.Contains(strings.ToLower(out), term) {
			t.Fatalf("%s refusal names the term %s:\n%s", what, term, out)
		}
	}
}

// TestGitHookCommitRefused is SP-045.1 a2: a commit containing a term is
// refused, one without passes, and the hook orch's chained still runs.
func TestGitHookCommitRefused(t *testing.T) {
	gitHookSnapshot(t, "")
	dir, marks := gitHookRepo(t)

	// A term in the message.
	out, err := gitIn(dir, "commit", "-q", "--allow-empty", "-m", "ask Zephyr about it")
	refusedFor(t, "a term in the message", out, err, "globex")
	// Reason: the chained commit-msg hook runs first, since it may add to
	// the message, which orch then checks as it will be committed.
	if m := readMarks(t, marks); !strings.HasPrefix(m, "commit-msg ") {
		t.Fatalf("the chained hook did not run before the check: %q", m)
	}

	// A term in a staged file, with an innocent message; and with commit -a.
	writeFile(t, filepath.Join(dir, "notes.txt"), "the falcon project\n")
	mustGit(t, dir, "add", "notes.txt")
	out, err = gitIn(dir, "commit", "-q", "-m", "add notes")
	refusedFor(t, "a term in a staged file", out, err, "acme")
	writeFile(t, filepath.Join(dir, "notes.txt"), "plain notes\n")
	mustGit(t, dir, "add", "notes.txt")

	// A term the chained hook adds to the message is caught too.
	hook := filepath.Join(dir, ".git", "hooks", "commit-msg.orch-prev")
	orig, _ := os.ReadFile(hook)
	if err := os.WriteFile(hook, append(orig, []byte("echo 'Ref: zephyr-1' >> \"$1\"\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = gitIn(dir, "commit", "-q", "-m", "add notes")
	refusedFor(t, "a term the chained hook adds", out, err, "globex")
	if err := os.WriteFile(hook, orig, 0o755); err != nil {
		t.Fatal(err)
	}
	readMarks(t, marks)

	// One without a term passes, and the user's hook runs with its file.
	mustGit(t, dir, "commit", "-q", "-m", "add notes")
	if m := readMarks(t, marks); !strings.HasPrefix(m, "commit-msg .git/COMMIT_EDITMSG") {
		t.Fatalf("chained commit-msg hook marks = %q", m)
	}

	writeFile(t, filepath.Join(dir, "notes.txt"), "now about falcon\n")
	out, err = gitIn(dir, "commit", "-q", "-a", "-m", "update")
	refusedFor(t, "commit -a with a term in a tracked change", out, err, "acme")
}

// TestGitHookPushRefused: a term in a commit made without the hooks
// (--no-verify) is refused at push; a clean push passes and the user's
// pre-push hook gets git's arguments and input.
func TestGitHookPushRefused(t *testing.T) {
	gitHookSnapshot(t, "")
	dir, marks := gitHookRepo(t)
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	mustGit(t, dir, "push", "-q", "origin", "main")
	if m := readMarks(t, marks); !strings.Contains(m, "pre-push origin ") || !strings.Contains(m, "refs/heads/main ") {
		t.Fatalf("chained pre-push marks = %q", m)
	}

	writeFile(t, filepath.Join(dir, "a.txt"), "zephyr\n")
	mustGit(t, dir, "add", "a.txt")
	mustGit(t, dir, "commit", "-q", "--no-verify", "-m", "sneak")
	readMarks(t, marks)
	out, err := gitIn(dir, "push", "-q", "origin", "main")
	refusedFor(t, "a pushed commit with a term", out, err, "globex")
	if m := readMarks(t, marks); m != "" {
		t.Fatalf("the chained hook ran after a refusal: %q", m)
	}

	// A tag of it is refused too, and so is a push to the URL itself.
	mustGit(t, dir, "tag", "v1")
	out, err = gitIn(dir, "push", "-q", "origin", "v1")
	refusedFor(t, "a pushed tag", out, err, "globex")
	bare := strings.TrimSpace(mustGit(t, dir, "remote", "get-url", "--push", "origin"))
	out, err = gitIn(dir, "push", "-q", bare, "main")
	refusedFor(t, "a push to a URL", out, err, "globex")

	// Once the commit is gone, the push passes; deleting a ref sends nothing.
	mustGit(t, dir, "tag", "-d", "v1")
	mustGit(t, dir, "reset", "-q", "--hard", "HEAD~1")
	writeFile(t, filepath.Join(dir, "a.txt"), "plain\n")
	mustGit(t, dir, "add", "a.txt")
	mustGit(t, dir, "commit", "-q", "-m", "plain")
	mustGit(t, dir, "push", "-q", "origin", "main")
	mustGit(t, dir, "push", "-q", "origin", "main:side")
	mustGit(t, dir, "push", "-q", "origin", ":side")
}

// TestGitHookSwitchAndPolicy: the terms layer switched off allows; with
// nothing configured the hooks allow; an unreadable snapshot refuses.
func TestGitHookSwitchAndPolicy(t *testing.T) {
	gitHookSnapshot(t, spaces.SwitchOff)
	dir, _ := gitHookRepo(t)
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "zephyr while off")

	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "empty"))
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "zephyr with no spaces")

	if err := os.MkdirAll(paths.GuardDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, spaces.GuardSnapshotPath(paths.GuardDir()), "{not json")
	out, err := gitIn(dir, "commit", "-q", "--allow-empty", "-m", "plain")
	if err == nil || !strings.Contains(out, "rule outbound_unchecked") {
		t.Fatalf("unreadable snapshot: %v\n%s", err, out)
	}
}

func TestGitHookCmdErrors(t *testing.T) {
	cf := factory(&fakeClient{}, nil)
	for _, args := range [][]string{
		{"guard", "git-hook"},
		{"guard", "git-hook", "--install", "--uninstall"},
		{"guard", "git-hook", "--install", "--repo", t.TempDir()},
		{"guard", "git-hook", "run", "post-commit"},
		{"guard", "git-hook", "run", "commit-msg"},
		{"guard", "git-hook", "run", "pre-push", "origin"},
	} {
		if _, err := run(cf, "", args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	if _, err := run(cf, "garbage\n", "guard", "git-hook", "run", "pre-push", "origin", "https://github.com/acme/app"); err == nil ||
		!strings.Contains(err.Error(), "refused git push") {
		t.Errorf("garbage pre-push input: %v", err)
	}
}

// TestGitHookCmdDryRun: without --apply nothing is written; --hooks-dir
// names a shared hooks directory.
func TestGitHookCmdDryRun(t *testing.T) {
	cf := factory(&fakeClient{}, nil)
	shared := filepath.Join(t.TempDir(), "hooks")
	out, err := run(cf, "", "guard", "git-hook", "--install", "--hooks-dir", shared, "--orch", "/a/orch")
	if err != nil || !strings.Contains(out, "DRY RUN") || !strings.Contains(out, shared) {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatalf("dry run created %s", shared)
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if _, err := run(cf, "", "guard", "git-hook", "--install", "--apply", "--hooks-dir", shared); err != nil {
		t.Fatal(err)
	}
	out, err = run(cf, "", "guard", "git-hook", "--uninstall", "--apply", "--hooks-dir", shared)
	if err != nil || !strings.Contains(out, "2 hook(s) changed") {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
}

// TestGitHookRunInProcess runs the hook command in this process, as git
// would from the work tree's top: a commit message with a term and a push
// of a commit with one are refused, clean ones pass.
func TestGitHookRunInProcess(t *testing.T) {
	gitHookSnapshot(t, "")
	dir, _ := gitHookRepo(t)
	t.Chdir(dir)
	cf := factory(&fakeClient{}, nil)
	writeFile(t, filepath.Join(dir, ".git", "COMMIT_EDITMSG"), "note for zephyr\n")
	_, err := run(cf, "", "guard", "git-hook", "run", "commit-msg", "--", ".git/COMMIT_EDITMSG")
	if err == nil || !strings.Contains(err.Error(), "outbound_term") || strings.Contains(err.Error(), "zephyr") {
		t.Fatalf("commit-msg with a term: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".git", "COMMIT_EDITMSG"), "plain\n")
	if _, err := run(cf, "", "guard", "git-hook", "run", "commit-msg", "--", ".git/COMMIT_EDITMSG"); err != nil {
		t.Fatalf("clean commit-msg: %v", err)
	}

	writeFile(t, filepath.Join(dir, "a.txt"), "falcon\n")
	mustGit(t, dir, "add", "a.txt")
	mustGit(t, dir, "commit", "-q", "--no-verify", "-m", "x")
	head := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
	zero := strings.Repeat("0", 40)
	in := "refs/heads/main " + head + " refs/heads/main " + zero + "\n"
	_, err = run(cf, in, "guard", "git-hook", "run", "pre-push", "--", "origin", "https://github.com/acme/app.git")
	if err == nil || !strings.Contains(err.Error(), `space "acme"`) {
		t.Fatalf("pre-push with a term: %v", err)
	}
	if _, err := run(cf, "(delete) "+zero+" refs/heads/x "+head+"\n", "guard", "git-hook", "run", "pre-push", "--", "origin", "u"); err != nil {
		t.Fatalf("deletion: %v", err)
	}

	// The repository's hooks directory is found from the current directory.
	out, err := run(cf, "", "guard", "git-hook", "--install", "--orch", "/a/orch")
	if err != nil || !strings.Contains(out, filepath.Join(dir, ".git", "hooks")) {
		t.Fatalf("dry run from the repo: %v\n%s", err, out)
	}
}
