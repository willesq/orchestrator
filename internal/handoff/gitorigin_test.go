package handoff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindCheckout(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "code", "acme", "widget")
	writeFile(t, filepath.Join(main, ".git", "config"),
		"[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/fork.git\n[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n")
	doc := filepath.Join(main, "docs", "PLAN.md")
	writeFile(t, doc, "plan")
	co, ok := FindCheckout(doc)
	if !ok || co.Root != main || co.Owner != "acme" || co.Repo != "widget" {
		t.Fatalf("main checkout = %+v, %v", co, ok)
	}

	// A linked worktree resolves to its main repository's origin.
	wt := filepath.Join(root, "code", "acme", "widget.worktrees", "feat", "x")
	gitDir := filepath.Join(main, ".git", "worktrees", "x")
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..\n")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")
	writeFile(t, filepath.Join(gitDir, "gitdir"), filepath.Join(wt, ".git")+"\n")
	co, ok = FindCheckout(filepath.Join(wt, "README.md"))
	if !ok || co.Root != wt || co.Owner != "acme" || co.Repo != "widget" {
		t.Fatalf("worktree = %+v, %v", co, ok)
	}

	// A relative gitdir, an https origin and no commondir.
	other := filepath.Join(root, "other")
	writeFile(t, filepath.Join(root, "other-git", "config"), "[core]\n\tworktree = ../other\n[remote \"origin\"]\n\turl = https://x-access-token@github.com/Acme/Tool/\n")
	writeFile(t, filepath.Join(other, ".git"), "gitdir: ../other-git")
	if co, ok := FindCheckout(filepath.Join(other, "a.md")); !ok || co.Owner != "Acme" || co.Repo != "Tool" {
		t.Fatalf("relative gitdir = %+v, %v", co, ok)
	}
	// The same git directory shipped inside the tree (an archive or a copy)
	// is not believed, whatever its core.worktree says.
	inTree := filepath.Join(root, "in-tree")
	writeFile(t, filepath.Join(inTree, ".gitdir-real", "config"), "[core]\n\tworktree = ..\n[remote \"origin\"]\n\turl = https://github.com/Acme/Tool\n")
	writeFile(t, filepath.Join(inTree, ".git"), "gitdir: .gitdir-real")
	if co, ok := FindCheckout(filepath.Join(inTree, "a.md")); !ok || !co.Anonymous || !strings.Contains(co.AnonymousWhy, "inside the checkout") {
		t.Fatalf("in-tree gitdir = %+v, %v", co, ok)
	}

	// A non-GitHub origin is a checkout with no owner.
	gl := filepath.Join(root, "gitlab")
	writeFile(t, filepath.Join(gl, ".git", "config"), "[remote \"origin\"]\n\turl = git@gitlab.com:a/b.git\n")
	if co, ok := FindCheckout(filepath.Join(gl, "a.md")); !ok || co.Owner != "" {
		t.Fatalf("gitlab = %+v, %v", co, ok)
	}

	// A malformed .git file, and no repository at all.
	bad := filepath.Join(root, "bad")
	writeFile(t, filepath.Join(bad, ".git"), "not a pointer")
	if co, ok := FindCheckout(filepath.Join(bad, "a.md")); !ok || !co.Anonymous || co.Owner != "" {
		t.Fatal("a malformed .git is not an anonymous checkout")
	}
	if _, ok := FindCheckout(filepath.Join(t.TempDir(), "x", "a.md")); ok {
		t.Fatal("found a checkout where there is none")
	}
}

func TestGitHubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:acme/widget.git":         "acme/widget",
		"ssh://git@github.com/acme/widget":       "acme/widget",
		"https://github.com/acme/widget.git":     "acme/widget",
		"https://github.com/acme/my.repo":        "acme/my.repo",
		"https://github.com/acme/widget/extra":   "/",
		"https://github.example.com/acme/widget": "/",
		"":                                       "/",
	} {
		o, r := githubRepo(url)
		if o+"/"+r != want {
			t.Errorf("githubRepo(%q) = %s/%s, want %s", url, o, r, want)
		}
	}
}

func TestFindCheckoutDoesNotBlockOnAFIFO(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "real")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscallMkfifo(filepath.Join(gitDir, "config")); err != nil {
		t.Skip("mkfifo:", err)
	}
	writeFile(t, filepath.Join(root, "wt", ".git"), "gitdir: "+gitDir)
	co, ok := FindCheckout(filepath.Join(root, "wt", "a.md"))
	if !ok || co.Owner != "" {
		t.Fatalf("fifo config = %+v, %v", co, ok)
	}
	if o, r := githubRepo("https://github.com/../x"); o != "" || r != "" {
		t.Fatal("dot owner accepted")
	}
}

func TestCheckoutBranchesAndCommondirLayout(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "widget")
	writeFile(t, filepath.Join(main, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n")
	writeFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(main, ".git", "refs", "remotes", "origin", "HEAD"), "ref: refs/remotes/origin/main\n")
	co, _ := FindCheckout(filepath.Join(main, "a.md"))
	if co.Branch != "main" || co.DefaultBranch != "main" || !co.OnDefaultBranch() {
		t.Fatalf("main = %+v", co)
	}
	// A linked worktree on a feature branch shares origin and default.
	wtGit := filepath.Join(main, ".git", "worktrees", "feat")
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/feat/x\n")
	wt := filepath.Join(root, "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")
	writeFile(t, filepath.Join(wtGit, "gitdir"), filepath.Join(wt, ".git")+"\n")
	co, _ = FindCheckout(filepath.Join(wt, "a.md"))
	if co.Owner != "acme" || co.Branch != "feat/x" || co.OnDefaultBranch() {
		t.Fatalf("worktree = %+v", co)
	}
	// A commondir pointing outside the worktrees layout borrows no identity.
	evil := filepath.Join(root, "evil")
	writeFile(t, filepath.Join(evil, "config"), "[remote \"origin\"]\n\turl = https://github.com/victim/repo\n")
	spoof := filepath.Join(root, "spoof")
	writeFile(t, filepath.Join(spoof, "gd", "commondir"), "../../evil\n")
	writeFile(t, filepath.Join(spoof, ".git"), "gitdir: gd\n")
	co, ok := FindCheckout(filepath.Join(spoof, "a.md"))
	if !ok || co.Owner != "" {
		t.Fatalf("commondir spoof = %+v, %v", co, ok)
	}
	// Detached HEAD, and no recorded default.
	if (Checkout{}).OnDefaultBranch() || !(Checkout{Branch: "master", DefaultBranch: "master"}).OnDefaultBranch() || (Checkout{Branch: "dev"}).OnDefaultBranch() {
		t.Fatal("OnDefaultBranch")
	}
	if symref(filepath.Join(root, "none"), "refs/") != "" {
		t.Fatal("symref of a missing file")
	}
	writeFile(t, filepath.Join(root, "bad"), "ref: refs/tags/v1\n")
	if symref(filepath.Join(root, "bad"), "refs/heads/") != "" {
		t.Fatal("symref outside the prefix")
	}
}

func TestGitdirPointerMustPointBack(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	writeFile(t, filepath.Join(victim, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n")
	writeFile(t, filepath.Join(victim, ".git", "HEAD"), "ref: refs/heads/main\n")
	// A .git file naming another repository's git directory borrows nothing.
	w := filepath.Join(root, "w")
	writeFile(t, filepath.Join(w, ".git"), "gitdir: "+filepath.Join(victim, ".git")+"\n")
	if co, ok := CheckoutAt(w); !ok || !co.Anonymous || co.Owner != "" || co.OnDefaultBranch() {
		t.Fatalf("borrowed gitdir = %+v, %v", co, ok)
	}
	// A worktrees entry whose gitdir file points elsewhere borrows nothing either.
	wtGit := filepath.Join(victim, ".git", "worktrees", "x")
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(wtGit, "gitdir"), "/somewhere/else/.git\n")
	w2 := filepath.Join(root, "w2")
	writeFile(t, filepath.Join(w2, ".git"), "gitdir: "+wtGit+"\n")
	if co, _ := CheckoutAt(w2); !co.Anonymous || co.Owner != "" {
		t.Fatalf("no back-pointer = %+v", co)
	}
	// CheckoutAt never looks above its directory.
	if _, ok := CheckoutAt(filepath.Join(victim, "docs")); ok {
		t.Fatal("CheckoutAt walked up")
	}
}

func TestDefaultBranchFallbacks(t *testing.T) {
	repo := func(t *testing.T, head string, files map[string]string) Checkout {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".git", "HEAD"), head)
		for name, body := range files {
			writeFile(t, filepath.Join(root, ".git", name), body)
		}
		co, _ := CheckoutAt(root)
		return co
	}
	cases := []struct {
		name, head string
		files      map[string]string
		ok         bool
		reason     string
	}{
		// A lone local branch is not the default: a single-branch or PR clone has one.
		{"sole local branch", "ref: refs/heads/trunk\n", map[string]string{"refs/heads/trunk": "x"}, false, "default branch is unknown"},
		{"packed sole branch", "ref: refs/heads/develop\n", map[string]string{"packed-refs": "# pack\nabc refs/heads/develop\n"}, false, "default branch is unknown"},
		{"init.defaultBranch", "ref: refs/heads/trunk\n", map[string]string{"refs/heads/trunk": "x", "refs/heads/feat": "x",
			"config": "[init]\n\tdefaultBranch = trunk\n"}, true, ""},
		{"main exists", "ref: refs/heads/feat\n", map[string]string{"refs/heads/main": "x", "refs/heads/feat": "x"}, false, "not the default branch main"},
		{"unknown", "ref: refs/heads/a\n", map[string]string{"refs/heads/a": "x", "refs/heads/b": "x"}, false, "default branch is unknown"},
		{"detached", "0123456789abcdef\n", nil, false, "detached"},
		{"reftable", "ref: refs/heads/.invalid\n", nil, false, "reftable"},
		{"origin HEAD wins", "ref: refs/heads/main\n", map[string]string{"refs/heads/main": "x", "refs/remotes/origin/HEAD": "ref: refs/remotes/origin/trunk\n"}, false, "not the default branch trunk"},
	}
	for _, c := range cases {
		co := repo(t, c.head, c.files)
		ok, reason := co.DefaultBranchState()
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: %v %q (%+v)", c.name, ok, reason, co)
		}
	}
	if ok, r := (Checkout{Anonymous: true}).DefaultBranchState(); ok || !strings.Contains(r, "layout") {
		t.Error("anonymous")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git %v: %v\n%s", args, err, out)
	}
}

// Layouts real git makes: relative worktree pointers, a separate git dir,
// and a single-branch clone of a feature branch.
func TestRealGitLayouts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	up := filepath.Join(root, "up")
	_ = os.MkdirAll(up, 0o755)
	gitCmd(t, up, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(up, "a.md"), "a")
	gitCmd(t, up, "add", "-A")
	gitCmd(t, up, "commit", "-q", "--no-gpg-sign", "-m", "1")
	gitCmd(t, up, "branch", "feat")

	rel := filepath.Join(root, "wt-rel")
	gitCmd(t, up, "worktree", "add", "-q", "--relative-paths", rel, "feat")
	co, ok := CheckoutAt(rel)
	if !ok || co.Anonymous || !co.Linked || co.Branch != "feat" {
		t.Fatalf("relative worktree = %+v", co)
	}

	sep := filepath.Join(root, "sep")
	_ = os.MkdirAll(sep, 0o755)
	gitCmd(t, sep, "init", "-q", "-b", "main", "--separate-git-dir", filepath.Join(root, "sep-git"))
	co, _ = CheckoutAt(sep)
	if ok, why := co.DefaultBranchState(); ok || !strings.Contains(why, "separate-git-dir") {
		t.Fatalf("separate git dir = %+v, %q", co, why)
	}

	single := filepath.Join(root, "single")
	gitCmd(t, root, "clone", "-q", "--single-branch", "--branch", "feat", up, single)
	co, _ = CheckoutAt(single)
	if ok, why := co.DefaultBranchState(); ok {
		t.Fatalf("a single-branch clone of feat counted as the default (%+v, %q)", co, why)
	}
}

func TestConfigInlineComments(t *testing.T) {
	if got := configScalar(" develop # team default"); got != "develop" {
		t.Fatalf("%q", got)
	}
	if got := configScalar(` "a # not a comment" ; c`); got != "a # not a comment" {
		t.Fatalf("%q", got)
	}
}
