package spacesettings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func localOf(checkout string) string {
	return filepath.Join(checkout, ".claude", "settings.local.json")
}

// TestSyncKeepsBackupOfFirstChange (SP-046.1.a2): the file as it was before
// orch's first change is backed up once, owner-only, and later syncs keep
// that original.
func TestSyncKeepsBackupOfFirstChange(t *testing.T) {
	w := newWorld(t)
	repo := gitRepo(t, filepath.Join(w.code, "a", "app"))
	writeFile(t, localOf(repo), userLocal)
	s := w.syncer()
	ctx := context.Background()

	rep := s.Sync(ctx, w.res, ChoiceOn)
	if len(rep.Problems) != 0 || !slices.Contains(rep.Written, repo) {
		t.Fatalf("first sync: %+v", rep)
	}
	backups, _ := filepath.Glob(filepath.Join(s.BackupDir, "*", "settings.local.json"))
	if len(backups) != 1 || readFile(t, backups[0]) != userLocal {
		t.Fatalf("backups %v", backups)
	}
	if info, _ := os.Stat(backups[0]); info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", info.Mode())
	}
	if got := readFile(t, filepath.Join(filepath.Dir(backups[0]), "checkout")); got != repo+"\n" {
		t.Errorf("backup names %q", got)
	}

	// The user edits the file; orch changes it again (the policy moved).
	writeFile(t, localOf(repo), strings.Replace(readFile(t, localOf(repo)), `"opus"`, `"sonnet"`, 1))
	if rep := s.Sync(ctx, w.res, Choice{SwitchedOff: true}); len(rep.Problems) != 0 || !slices.Contains(rep.Cleared, repo) {
		t.Fatalf("clear: %+v", rep)
	}
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Problems) != 0 {
		t.Fatalf("again: %+v", rep)
	}
	if readFile(t, backups[0]) != userLocal {
		t.Error("the original backup was replaced")
	}
}

// TestSyncIdempotent (SP-046.1.a2): a second sync writes nothing.
func TestSyncIdempotent(t *testing.T) {
	w := newWorld(t)
	repo := gitRepo(t, filepath.Join(w.code, "b", "svc"))
	s := w.syncer()
	ctx := context.Background()
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Written) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("first: %+v", rep)
	}
	before := readFile(t, localOf(repo))
	info1, _ := os.Stat(localOf(repo))
	rep := s.Sync(ctx, w.res, ChoiceOn)
	if len(rep.Written)+len(rep.Cleared)+len(rep.Ignored)+len(rep.Problems) != 0 {
		t.Errorf("second sync did something: %+v", rep)
	}
	info2, _ := os.Stat(localOf(repo))
	if readFile(t, localOf(repo)) != before || !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("second sync rewrote the file")
	}
	if info1.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v", info1.Mode())
	}
}

// TestSyncIgnoresEveryCheckout (SP-046.1.a3): after a sync git ignores the
// file in every checkout; orch adds an info/exclude line only where no rule
// covers it, never edits a .gitignore, and refuses a tracked file.
func TestSyncIgnoresEveryCheckout(t *testing.T) {
	w := newWorld(t)
	plain := gitRepo(t, filepath.Join(w.code, "a", "plain"))
	covered := gitRepo(t, filepath.Join(w.code, "b", "covered"))
	writeFile(t, filepath.Join(covered, ".gitignore"), ".claude/settings.local.json\n")
	git(t, covered, "add", ".gitignore")
	git(t, covered, "commit", "-q", "-m", "ignore")
	excluded := gitRepo(t, filepath.Join(w.code, "me", "excluded"))
	writeFile(t, filepath.Join(excluded, ".git", "info", "exclude"), "# mine\n.claude/")
	main := gitRepo(t, filepath.Join(w.code, "c", "main"))
	writeFile(t, filepath.Join(main, "README"), "x")
	git(t, main, "add", "README")
	git(t, main, "commit", "-q", "-m", "init")
	worktree := filepath.Join(w.code, "c", "main.worktrees", "feat")
	git(t, main, "worktree", "add", "-q", "-b", "feat", worktree)
	tracked := gitRepo(t, filepath.Join(w.code, "tracked"))
	writeFile(t, localOf(tracked), `{"model":"opus"}`)
	git(t, tracked, "add", "-f", ".claude/settings.local.json")
	git(t, tracked, "commit", "-q", "-m", "tracked")

	s := w.syncer()
	rep := s.Sync(context.Background(), w.res, ChoiceOn)
	if rep.Checkouts != 6 {
		t.Fatalf("found %d checkouts: %+v", rep.Checkouts, rep)
	}
	for _, c := range []string{plain, covered, excluded, main, worktree} {
		if _, err := os.Stat(localOf(c)); err != nil {
			t.Errorf("%s: no settings file: %v", c, err)
		}
		ok, err := s.Git.Ignored(context.Background(), c)
		if err != nil || !ok {
			t.Errorf("%s: not ignored (%v)", c, err)
		}
	}
	if got := readFile(t, filepath.Join(plain, ".git", "info", "exclude")); !strings.HasSuffix(got, "\n/.claude/settings.local.json\n") {
		t.Errorf("plain exclude = %q", got)
	}
	if got := readFile(t, filepath.Join(covered, ".gitignore")); got != ".claude/settings.local.json\n" {
		t.Errorf(".gitignore edited: %q", got)
	}
	if got := readFile(t, filepath.Join(covered, ".git", "info", "exclude")); strings.Contains(got, "settings.local.json") {
		t.Errorf("covered checkout got an exclude line: %q", got)
	}
	if got := readFile(t, filepath.Join(excluded, ".git", "info", "exclude")); got != "# mine\n.claude/" {
		t.Errorf("excluded checkout's exclude changed: %q", got)
	}
	// The worktree shares its main checkout's info/exclude.
	if got := readFile(t, filepath.Join(main, ".git", "info", "exclude")); strings.Count(got, "/.claude/settings.local.json") != 1 {
		t.Errorf("main exclude = %q", got)
	}
	if readFile(t, localOf(tracked)) != `{"model":"opus"}` {
		t.Error("tracked file written")
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], tracked) || !strings.Contains(rep.Problems[0], "tracked") {
		t.Errorf("problems = %v", rep.Problems)
	}
	if !slices.Contains(rep.Ignored, plain) || slices.Contains(rep.Ignored, covered) {
		t.Errorf("ignored = %v", rep.Ignored)
	}
}

// TestSyncPicksUpNewClone (SP-046.1.a4): a checkout cloned under a root
// after a sync gets its file at the next sync, and one that is removed
// leaves the state.
func TestSyncPicksUpNewClone(t *testing.T) {
	w := newWorld(t)
	s := w.syncer()
	ctx := context.Background()
	if rep := s.Sync(ctx, w.res, ChoiceOn); rep.Checkouts != 0 {
		t.Fatalf("%+v", rep)
	}
	src := gitRepo(t, filepath.Join(w.home, "src"))
	clone := filepath.Join(w.code, "b", "clone")
	git(t, w.home, "clone", "-q", src, clone)
	if rep := s.Sync(ctx, w.res, ChoiceOn); !slices.Contains(rep.Written, clone) {
		t.Fatalf("clone not written: %+v", rep)
	}
	if !strings.Contains(readFile(t, localOf(clone)), "Read(/"+filepath.Join(w.code, "a")+"/**)") {
		t.Error("clone lacks job a's deny")
	}
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	s.Sync(ctx, w.res, ChoiceOn)
	st, _, err := s.load()
	if err != nil || len(st.Checkouts) != 0 {
		t.Errorf("state after removal: %+v %v", st, err)
	}
}

// TestSandboxSwitch (SP-046.3.a1): switching the sandbox layer off takes
// orch's entries out of every file and leaves the user's; on restores them.
func TestSandboxSwitch(t *testing.T) {
	w := newWorld(t)
	withUser := gitRepo(t, filepath.Join(w.code, "a", "one"))
	writeFile(t, localOf(withUser), userLocal)
	fresh := gitRepo(t, filepath.Join(w.code, "b", "two"))
	public := gitRepo(t, filepath.Join(w.code, "oss-lib"))
	s := w.syncer()
	ctx := context.Background()

	on := s.Sync(ctx, w.res, ChoiceOn)
	if len(on.Written) != 3 || len(on.Problems) != 0 {
		t.Fatalf("on: %+v", on)
	}
	enforced := readFile(t, localOf(withUser))

	off := s.Sync(ctx, w.res, Choice{SwitchedOff: true})
	if len(off.Cleared) != 3 || len(off.Problems) != 0 {
		t.Fatalf("off: %+v", off)
	}
	if !sameMeaning(t, []byte(readFile(t, localOf(withUser))), []byte(userLocal)) {
		t.Errorf("user's file after off:\n%s", readFile(t, localOf(withUser)))
	}
	for _, c := range []string{fresh, public} {
		if _, err := os.Stat(localOf(c)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: orch's own file left after off: %v", c, err)
		}
	}

	again := s.Sync(ctx, w.res, ChoiceOn)
	if len(again.Written) != 3 || len(again.Problems) != 0 {
		t.Fatalf("on again: %+v", again)
	}
	if !sameMeaning(t, []byte(readFile(t, localOf(withUser))), []byte(enforced)) {
		t.Errorf("not restored:\n%s\nwant\n%s", readFile(t, localOf(withUser)), enforced)
	}

	// Without a policy (it was removed) orch's entries go too.
	if none := s.Sync(ctx, nil, ChoiceOn); len(none.Cleared) != 3 {
		t.Errorf("no policy: %+v", none)
	}
}

// TestSyncMovesUnusableAside: a .claude or settings file that is a link,
// or a file orch cannot use as it is, is moved aside (the link itself, never
// what it points at) and orch's own file written, so the checkout is never
// left without its rules.
func TestSyncMovesUnusableAside(t *testing.T) {
	w := newWorld(t)
	elsewhere := filepath.Join(w.home, "elsewhere")
	writeFile(t, filepath.Join(elsewhere, "settings.local.json"), "{}")
	dirLink := gitRepo(t, filepath.Join(w.code, "a", "dirlink"))
	if err := os.Symlink(elsewhere, filepath.Join(dirLink, ".claude")); err != nil {
		t.Fatal(err)
	}
	fileLink := gitRepo(t, filepath.Join(w.code, "a", "filelink"))
	if err := os.MkdirAll(filepath.Join(fileLink, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "settings.local.json"), localOf(fileLink)); err != nil {
		t.Fatal(err)
	}
	big := gitRepo(t, filepath.Join(w.code, "a", "big"))
	writeFile(t, localOf(big), `{"sandbox":{"enabled":false}}`+strings.Repeat(" ", 5<<20))
	rep := w.syncer().Sync(context.Background(), w.res, ChoiceOn)
	if len(rep.Problems) != 3 || len(rep.Written) != 3 {
		t.Errorf("report = %+v", rep)
	}
	if readFile(t, filepath.Join(elsewhere, "settings.local.json")) != "{}" {
		t.Error("wrote through a link")
	}
	for _, c := range []string{dirLink, fileLink, big} {
		info, err := os.Lstat(localOf(c))
		if err != nil || !info.Mode().IsRegular() || !strings.Contains(readFile(t, localOf(c)), `"enabled": true`) {
			t.Errorf("%s: no rules written: %v", c, err)
		}
	}
	if moved, _ := filepath.Glob(filepath.Join(dirLink, ".claude.orch-moved-*")); len(moved) != 1 {
		t.Errorf("moved .claude link: %v", moved)
	} else if l, err := os.Readlink(moved[0]); err != nil || l != elsewhere {
		t.Errorf("moved link points at %q (%v)", l, err)
	}
	if moved, _ := filepath.Glob(filepath.Join(big, ".claude", "settings.local.json.orch-moved-*")); len(moved) != 1 {
		t.Errorf("moved big file: %v", moved)
	}
	// A committed link, at .claude or at the file, is the repository's:
	// refused, never moved.
	for i, at := range []string{".claude", ".claude/settings.local.json"} {
		repo := gitRepo(t, filepath.Join(w.code, "a", fmt.Sprintf("committed%d", i)))
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, at)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(repo, at)); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-f", at)
		git(t, repo, "commit", "-q", "-m", "link")
		rep := w.syncer().Sync(context.Background(), w.res, ChoiceOn)
		if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, repo) && strings.Contains(p, "tracked") }) {
			t.Errorf("%s: problems %v", at, rep.Problems)
		}
		if info, err := os.Lstat(filepath.Join(repo, at)); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: tracked link moved: %v", at, err)
		}
	}
	// A git that cannot answer stops the move too.
	broken := gitRepo(t, filepath.Join(w.code, "a", "broken"))
	if err := os.Symlink(elsewhere, filepath.Join(broken, ".claude")); err != nil {
		t.Fatal(err)
	}
	bs := w.syncer()
	bs.Git = Git{Bin: filepath.Join(w.home, "no-git")}
	if rep := bs.Sync(context.Background(), w.res, ChoiceOn); !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, broken) }) {
		t.Errorf("no git: %v", rep.Problems)
	}
	if info, _ := os.Lstat(filepath.Join(broken, ".claude")); info.Mode()&os.ModeSymlink == 0 {
		t.Error("moved without asking git")
	}
	// Switched off, nothing is moved.
	off := gitRepo(t, filepath.Join(w.code, "a", "off"))
	if err := os.Symlink(elsewhere, filepath.Join(off, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := MoveAside(filepath.Join(w.home, "absent")); err == nil {
		t.Error("move aside in a missing checkout")
	}
	plain := gitRepo(t, filepath.Join(w.code, "a", "plain"))
	if _, err := MoveAside(plain); err == nil {
		t.Error("moved a missing settings file")
	}
	w.syncer().Sync(context.Background(), w.res, Choice{SwitchedOff: true})
	if info, err := os.Lstat(filepath.Join(off, ".claude")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("moved while switched off: %v", err)
	}
}

// TestSyncProblems: a placement error, a file orch cannot merge and an
// unreadable state file are reported, and nothing is written for them.
func TestSyncProblems(t *testing.T) {
	w := newWorld(t)
	bad := gitRepo(t, filepath.Join(w.code, "a", "bad"))
	writeFile(t, localOf(bad), `{"sandbox":"on"}`)
	gitRepo(t, filepath.Join(w.code, "b", "unplaceable"))
	s := w.syncer()
	s.Place = func(ctx context.Context, c string) (string, bool, error) {
		if strings.HasSuffix(c, "unplaceable") {
			return "", false, errors.New("lookup failed")
		}
		return w.placeByRoot(ctx, c)
	}
	rep := s.Sync(context.Background(), w.res, ChoiceOn)
	if len(rep.Problems) != 2 || !slices.Contains(rep.Written, bad) {
		t.Errorf("%+v", rep)
	}
	// Fail closed: the file orch could not merge into is kept as a backup
	// and replaced by orch's rules; switching off then removes the file.
	if got := readFile(t, localOf(bad)); !strings.Contains(got, `"enabled": true`) {
		t.Errorf("unmergeable file not replaced:\n%s", got)
	}
	kept, _ := filepath.Glob(filepath.Join(s.BackupDir, "*", "invalid-*.json"))
	if len(kept) != 1 || readFile(t, kept[0]) != `{"sandbox":"on"}` {
		t.Errorf("invalid backups %v", kept)
	}
	// orch's own rewritten file is never taken for the user's original.
	if st, _, _ := s.load(); !st.Checkouts[bad].BackedUp {
		t.Error("backed_up not recorded after the invalid file's backup")
	}
	if rep := s.Sync(context.Background(), w.res, Choice{SwitchedOff: true}); !slices.Contains(rep.Cleared, bad) {
		t.Errorf("off: %+v", rep)
	}
	if _, err := os.Stat(localOf(bad)); !os.IsNotExist(err) {
		t.Errorf("replaced file left after off: %v", err)
	}
	// Switched off, an unmergeable file is only reported.
	writeFile(t, localOf(bad), `{"sandbox":"on"}`)
	st, _, _ := s.load()
	st.Checkouts[bad] = fileState{Owned: Owned{Lists: map[string][]string{keyDenyRead: {"x"}}}}
	if err := s.save(st, nil); err != nil {
		t.Fatal(err)
	}
	if rep := s.Sync(context.Background(), w.res, Choice{SwitchedOff: true}); len(rep.Problems) != 1 || readFile(t, localOf(bad)) != `{"sandbox":"on"}` {
		t.Errorf("off with a bad file: %+v", rep)
	}

	writeFile(t, s.StateFile, "not json")
	if rep := s.Sync(context.Background(), w.res, ChoiceOn); len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "checkouts.json") {
		t.Errorf("bad state: %+v", rep)
	}
}

// TestSyncUnplacedCheckout: a checkout with no space gets denies only.
func TestSyncUnplacedCheckout(t *testing.T) {
	w := newWorld(t)
	repo := gitRepo(t, filepath.Join(w.code, "a", "conflict"))
	s := w.syncer()
	s.Place = func(context.Context, string) (string, bool, error) { return "", false, nil }
	if rep := s.Sync(context.Background(), w.res, ChoiceOn); len(rep.Written) != 1 {
		t.Fatalf("%+v", rep)
	}
	got := readFile(t, localOf(repo))
	if strings.Contains(got, "GH_CONFIG_DIR") || strings.Contains(got, "TMPDIR") || !strings.Contains(got, filepath.Join(w.code, "b")) {
		t.Errorf("unplaced file:\n%s", got)
	}
	if strings.Contains(got, `"`+filepath.Join(w.code, "a")+`"`) {
		t.Errorf("unplaced checkout denies its own root:\n%s", got)
	}
}

// TestSyncFailsClosed: a checkout orch wrote keeps its rules when the
// session in it renames .git (so discovery no longer finds it), when it
// cannot be placed, and when its space leaves the policy; they go only with
// the folder, the root or the switch.
func TestSyncFailsClosed(t *testing.T) {
	w := newWorld(t)
	repo := gitRepo(t, filepath.Join(w.code, "a", "app"))
	s := w.syncer()
	ctx := context.Background()
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Written) != 1 {
		t.Fatalf("%+v", rep)
	}
	enforced := readFile(t, localOf(repo))
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, ".git.x")); err != nil {
		t.Fatal(err)
	}
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Cleared) != 0 || readFile(t, localOf(repo)) != enforced {
		t.Errorf(".git renamed: %+v\n%s", rep, readFile(t, localOf(repo)))
	}
	if err := os.Rename(filepath.Join(repo, ".git.x"), filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	s.Place = func(context.Context, string) (string, bool, error) { return "", false, errors.New("down") }
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Cleared) != 0 || readFile(t, localOf(repo)) != enforced {
		t.Errorf("place error: %+v", rep)
	}
	// A new checkout that cannot be placed gets the strictest rules.
	fresh := gitRepo(t, filepath.Join(w.code, "a", "fresh"))
	s.Sync(ctx, w.res, ChoiceOn)
	if got := readFile(t, localOf(fresh)); !strings.Contains(got, filepath.Join(w.code, "b")) || strings.Contains(got, "GH_CONFIG_DIR") {
		t.Errorf("unplaceable new checkout:\n%s", got)
	}
	// Its root leaves the policy: the rules go.
	gone := newWorld(t).res
	if rep := s.Sync(ctx, gone, ChoiceOn); !slices.Contains(rep.Cleared, repo) {
		t.Errorf("root gone: %+v", rep)
	}
}

// TestCheckoutFragmentProtectsSettings: the session cannot change the
// settings that hold its rules, by shell or by file tool.
func TestCheckoutFragmentProtectsSettings(t *testing.T) {
	w := newWorld(t)
	repo := filepath.Join(w.code, "a", "app")
	f := CheckoutFragment(For(w.res, "joba", w.layout), repo)
	if !slices.Contains(f.Lists[keyDenyWrite], filepath.Join(repo, ".claude")) {
		t.Errorf("denyWrite = %v", f.Lists[keyDenyWrite])
	}
	if slices.Contains(f.Lists[keyDenyRead], filepath.Join(repo, ".claude")) {
		t.Error("the session cannot read its own settings")
	}
	for _, want := range []string{"Edit(/" + localOf(repo) + ")", "Edit(/" + filepath.Join(repo, ".claude", "settings.json") + ")"} {
		if !slices.Contains(f.Lists[keyPermDeny], want) {
			t.Errorf("permissions.deny lacks %s", want)
		}
	}
	if g := CheckoutFragment(Rules{}, "/x[1]"); len(g.Lists[keyPermDeny]) != 0 {
		t.Errorf("glob path got rules: %v", g.Lists[keyPermDeny])
	}
}
