package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit answers the git calls the search makes, from a temp layout only:
// the global core.hooksPath, and per repository its git dir and hooks path.
type fakeGit struct {
	global    string
	hooksPath map[string]string // repo -> core.hooksPath
	fail      map[string]bool   // repo -> git errors
	calls     []string
}

func (g *fakeGit) run(_ context.Context, args ...string) (string, error) {
	g.calls = append(g.calls, strings.Join(args, " "))
	if len(args) > 1 && args[0] == "config" {
		if args[1] == "--global" && g.global != "" {
			return g.global + "\n", nil
		}
		return "", errors.New("exit status 1")
	}
	// -c core.fsmonitor=false -C <repo> rev-parse ...
	if len(args) < 4 || args[2] != "-C" {
		return "", errors.New("unexpected git call")
	}
	repo := args[3]
	if g.fail[repo] {
		return "", errors.New("not a git repository")
	}
	hooks := filepath.Join(repo, ".git", "hooks")
	if p, ok := g.hooksPath[repo]; ok {
		hooks = p
	}
	return filepath.Join(repo, ".git") + "\n" + hooks + "\n", nil
}

// fakeRepo makes a repository work tree with a .git folder.
func fakeRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	return dir
}

// oldInstall puts orch's hooks in dir as `orch guard git-hook --install`
// did before the registry: nothing is recorded.
func oldInstall(t *testing.T, dir string, withUserHook bool) map[string]string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withUserHook {
		if err := os.WriteFile(filepath.Join(dir, "commit-msg"), []byte(userHook), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	before := hookTree(t, dir)
	applyGitHooks(t, dir, testBins().Orch, false, t.TempDir())
	return before
}

func TestUninstallFindsGitHooksInstalledBeforeTheRegistry(t *testing.T) {
	h := newTestHome(t)
	elsewhere := t.TempDir()

	// A registered repository, with a hook of the user's that orch chained.
	registered := fakeRepo(t, filepath.Join(elsewhere, "registered"))
	regHooks := filepath.Join(registered, ".git", "hooks")
	regBefore := oldInstall(t, regHooks, true)
	// A checkout under a known root, with no hook before orch's.
	rooted := fakeRepo(t, filepath.Join(h.home, "code", "acme", "app"))
	rootHooks := filepath.Join(rooted, ".git", "hooks")
	rootBefore := oldInstall(t, rootHooks, false)
	// A repository whose core.hooksPath points out of its git folder.
	pathed := fakeRepo(t, filepath.Join(h.home, "code", "acme", "pathed"))
	shared := filepath.Join(elsewhere, "shared-hooks")
	sharedBefore := oldInstall(t, shared, true)
	// The global core.hooksPath.
	global := filepath.Join(elsewhere, "global-hooks")
	globalBefore := oldInstall(t, global, false)
	// A --hooks-dir install found only by --scan, two folders down.
	scanRoot := filepath.Join(elsewhere, "scan")
	scanned := filepath.Join(scanRoot, "team", "hooks")
	scannedBefore := oldInstall(t, scanned, true)

	// What must be left alone: a look-alike without orch's marker, a hook
	// of orch's behind a symlinked folder, and a chained hook orphaned when
	// orch's hook was removed by hand.
	lookalike := fakeRepo(t, filepath.Join(h.home, "code", "acme", "lookalike"))
	lookHook := filepath.Join(lookalike, ".git", "hooks", "commit-msg")
	writeFile(t, lookHook, "#!/bin/sh\n# my wrapper\nexec orch guard git-hook run commit-msg -- \"$@\"\n")
	hidden := fakeRepo(t, filepath.Join(elsewhere, "hidden"))
	hiddenHooks := filepath.Join(hidden, ".git", "hooks")
	hiddenBefore := oldInstall(t, hiddenHooks, false)
	hiddenAfterInstall := hookTree(t, hiddenHooks)
	if err := os.Symlink(hidden, filepath.Join(h.home, "code", "acme", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "hidden"), filepath.Join(scanRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	orphan := fakeRepo(t, filepath.Join(h.home, "code", "acme", "orphan"))
	orphanPrev := filepath.Join(orphan, ".git", "hooks", "pre-push"+GitHookPrevSuffix)
	writeFile(t, orphanPrev, userHook)
	_ = hiddenBefore

	git := &fakeGit{global: global, hooksPath: map[string]string{pathed: shared}}
	m := h.machine(&fakeRun{})
	m.Git = git.run
	m.GitHookPlaces = func() GitHookPlaces {
		return GitHookPlaces{Repos: []string{registered, filepath.Join(elsewhere, "gone")}, Roots: []string{filepath.Join(h.home, "code")}}
	}
	m.GitHookScan = []string{scanRoot}
	units := FullUnits(m, nil)

	// The dry run lists every removal and changes nothing.
	snapshot := func() map[string]map[string]string {
		out := map[string]map[string]string{}
		for _, d := range []string{regHooks, rootHooks, shared, global, scanned, hiddenHooks, filepath.Dir(lookHook), filepath.Dir(orphanPrev)} {
			out[d] = hookTree(t, d)
		}
		return out
	}
	pre := snapshot()
	var out bytes.Buffer
	if err := UninstallPlan("all", units, true).Print(&out); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{regHooks, rootHooks, shared, global, scanned} {
		if !strings.Contains(out.String(), "from "+d+"\n") {
			t.Errorf("dry run does not list %s:\n%s", d, out.String())
		}
	}
	for _, want := range []string{"global core.hooksPath", "--scan " + scanRoot, lookHook + " runs `orch guard git-hook` but has no orch marker",
		orphanPrev + " is a hook an orch hook chained", "mv "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "orch uninstall --scan <folder>") {
		t.Error("the --scan hint is printed although --scan was given")
	}
	for d, tree := range snapshot() {
		if !sameTree(pre[d], tree) {
			t.Errorf("dry run changed %s", d)
		}
	}

	// Apply removes exactly orch's hooks and puts the chained ones back.
	applyPlan(t, UninstallPlan("all", FullUnits(m, nil), true), h)
	for d, want := range map[string]map[string]string{regHooks: regBefore, rootHooks: rootBefore, shared: sharedBefore,
		global: globalBefore, scanned: scannedBefore} {
		if got := hookTree(t, d); !sameTree(want, got) {
			t.Errorf("%s not restored: %v", d, treeNames(got))
		}
	}
	if got := hookTree(t, hiddenHooks); !sameTree(hiddenAfterInstall, got) {
		t.Error("the search followed a symlink and changed the hooks behind it")
	}
	if b, _ := os.ReadFile(lookHook); !strings.Contains(string(b), "my wrapper") {
		t.Error("a look-alike without orch's marker was changed")
	}
	if _, err := os.Stat(orphanPrev); err != nil {
		t.Error("an orphaned chained hook was moved or removed")
	}
	if n, err := UninstallPlan("all", FullUnits(m, nil), true).Pending(); err != nil || n != 0 {
		t.Errorf("second uninstall has %d pending, %v", n, err)
	}

	// --no-git-hooks searches nothing.
	m.NoGitHooks = true
	u := m.gitHookDiscoveryUnit()
	if len(u.UndoSteps())+len(u.Leftovers()) != 0 {
		t.Error("--no-git-hooks searched for hooks")
	}
}

func TestGitHookSearchWithoutGitAndBounds(t *testing.T) {
	h := newTestHome(t)
	// Without git, a work tree's .git/hooks is still found.
	repo := fakeRepo(t, filepath.Join(h.home, "code", "o", "r"))
	hooks := filepath.Join(repo, ".git", "hooks")
	before := oldInstall(t, hooks, true)
	m := h.machine(&fakeRun{})
	m.GitHookPlaces = func() GitHookPlaces { return GitHookPlaces{Roots: []string{filepath.Join(h.home, "code")}} }
	applyPlan(t, UninstallPlan("all", FullUnits(m, nil), true), h)
	if !sameTree(before, hookTree(t, hooks)) {
		t.Errorf("hooks not restored: %v", treeNames(hookTree(t, hooks)))
	}

	// A root or --scan that is a symlink, relative, missing, or a file is
	// reported or skipped, never followed.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(h.home, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(h.home, "file")
	writeFile(t, file, "x")
	f := &hookFinder{budget: maxFindEntries, found: map[string]string{}}
	f.walk(link, scanDepth, true, "s")
	f.walk("relative", scanDepth, true, "s")
	f.walk(filepath.Join(h.home, "missing"), scanDepth, true, "s")
	f.walk(filepath.Join(h.home, "missing"), rootDepth, false, "r")
	f.walk(file, scanDepth, true, "s")
	notes := strings.Join(f.notes, "\n")
	for _, want := range []string{link + " is a symlink; not followed", "relative is not an absolute path", "could not search " + filepath.Join(h.home, "missing")} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if len(f.order) != 0 {
		t.Errorf("found %v", f.order)
	}

	// The entry budget stops a search and says so.
	f = &hookFinder{budget: 1, found: map[string]string{}}
	f.walk(h.home, scanDepth, true, "s")
	if !strings.Contains(strings.Join(f.notes, "\n"), "stopped after") {
		t.Errorf("no truncation note: %v", f.notes)
	}

	// A repository git cannot name hooks for is reported.
	g := &fakeGit{fail: map[string]bool{repo: true}}
	f = &hookFinder{git: g.run, budget: maxFindEntries, found: map[string]string{}}
	f.repo(repo, "r")
	f.repo("relative", "r")
	if !strings.Contains(strings.Join(f.notes, "\n"), "could not ask git for the hooks folder of "+repo) {
		t.Errorf("notes = %v", f.notes)
	}
	// Under a relocated home only the global config is asked, never --system.
	f = &hookFinder{git: g.run, budget: maxFindEntries, found: map[string]string{}}
	f.globalHooksPaths(true)
	f.globalHooksPaths(false)
	if got := strings.Join(g.calls, "\n"); strings.Count(got, "--system") != 1 {
		t.Errorf("git calls:\n%s", got)
	}
}

func TestInspectHookDirReportsLookAlikes(t *testing.T) {
	dir := t.TempDir()
	// A link to orch's hook, and a non-regular hook.
	target := filepath.Join(t.TempDir(), "orch-hook")
	writeFile(t, target, string(GitHookScript("commit-msg", "/opt/orch/orch")))
	if err := os.Symlink(target, filepath.Join(dir, "commit-msg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "pre-push"), 0o755); err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(inspectHookDir(dir), "\n")
	for _, want := range []string{"is a link to an orch git hook", "not a regular file"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	// A chained hook beside a hook that is no longer orch's.
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "commit-msg"), userHook)
	writeFile(t, filepath.Join(dir, "commit-msg"+GitHookPrevSuffix), userHook)
	if notes := strings.Join(inspectHookDir(dir), "\n"); !strings.Contains(notes, "both left alone") {
		t.Errorf("notes = %s", notes)
	}
	// A link to something else is not reported.
	dir = t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "commit-msg")); err != nil {
		t.Fatal(err)
	}
	if notes := inspectHookDir(dir); len(notes) != 0 {
		t.Errorf("notes = %v", notes)
	}
}

func TestDiscoveryReportsSymlinkedHooksDir(t *testing.T) {
	h := newTestHome(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "hooks")
	oldInstall(t, real, false)
	if err := os.Symlink(real, filepath.Join(repo, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	m := h.machine(&fakeRun{})
	m.GitHookPlaces = func() GitHookPlaces { return GitHookPlaces{Repos: []string{repo}} }
	u := m.gitHookDiscoveryUnit()
	if steps := u.UndoSteps(); len(steps) != 0 {
		t.Errorf("planned %d steps through a symlinked hooks folder", len(steps))
	}
	if notes := strings.Join(u.Leftovers(), "\n"); !strings.Contains(notes, "is a symlink; not followed") {
		t.Errorf("leftovers = %s", notes)
	}
}

func TestScanLeavesHookCopiesOutsideHooksFolders(t *testing.T) {
	scan := t.TempDir()
	// A dotfiles repository that tracks a copy of orch's hook.
	copyDir := filepath.Join(scan, "dotfiles", "githooks")
	oldInstall(t, copyDir, true)
	before := hookTree(t, copyDir)
	f := &hookFinder{budget: maxFindEntries, found: map[string]string{}}
	f.walk(scan, scanDepth, true, "s")
	if len(f.order) != 0 {
		t.Errorf("found %v", f.order)
	}
	if !strings.Contains(strings.Join(f.notes, "\n"), copyDir+" holds a copy of an orch git hook but is not a hooks folder") {
		t.Errorf("notes = %v", f.notes)
	}
	if !sameTree(before, hookTree(t, copyDir)) {
		t.Error("the search changed a hook copy")
	}
}

func TestRepoNeedsTwoAbsolutePathsFromGit(t *testing.T) {
	repo := fakeRepo(t, filepath.Join(t.TempDir(), "r"))
	for _, answer := range []string{"", "relative\n/abs\n", "/a\n/b\n/c\n"} {
		git := func(context.Context, ...string) (string, error) { return answer, nil }
		f := &hookFinder{git: git, budget: maxFindEntries, found: map[string]string{}}
		f.repo(repo, "r")
		if len(f.order) != 1 || !strings.Contains(strings.Join(f.notes, "\n"), "git named no usable hooks folder") {
			t.Errorf("answer %q: found %v, notes %v", answer, f.order, f.notes)
		}
	}
	// Past the cap, git is not asked and the note says so once.
	f := &hookFinder{git: (&fakeGit{}).run, budget: maxFindEntries, found: map[string]string{}, gitCalls: maxGitCalls}
	f.repo(repo, "r")
	f.repo(repo, "r")
	if n := strings.Count(strings.Join(f.notes, "\n"), "and no more"); n != 1 {
		t.Errorf("cap notes = %v", f.notes)
	}
}
