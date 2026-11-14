package spacesettings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shortTmp is a temp folder under /tmp, short enough for Claude Code to use
// a space's folder in it (claudeTmpMax), removed after the test.
func shortTmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "oe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// TestShortTmpLayout (SP-046.1.a6, RC-184): with a Tmp root each space's
// TMPDIR is a folder there short enough that Claude Code's sandboxed
// commands use it; a name too long for that gets a hashed folder, distinct
// per space; every other space's folder is denied by its written and its
// real path, even before it exists.
func TestShortTmpLayout(t *testing.T) {
	w := newWorld(t)
	w.layout.Tmp = filepath.Join(shortTmp(t), "r")
	a, b := For(w.res, "joba", w.layout), For(w.res, "jobb", w.layout)
	if a.TmpDir != filepath.Join(w.layout.Tmp, "joba") {
		t.Fatalf("TmpDir = %s", a.TmpDir)
	}
	if got, want := ClaudeSandboxTmp(a.TmpDir), filepath.Join(a.TmpDir, fmt.Sprintf("claude-%d", os.Getuid())); got != want {
		t.Errorf("ClaudeSandboxTmp = %s, want %s", got, want)
	}
	if !slices.Contains(a.Deny, b.TmpDir) || slices.Contains(a.Deny, a.TmpDir) {
		t.Errorf("deny %v: want jobb's tmp %s, not joba's", a.Deny, b.TmpDir)
	}
	// /tmp is /private/tmp on macOS; the root does not exist yet.
	if real, _ := filepath.EvalSymlinks("/tmp"); real != "/tmp" {
		want := filepath.Join(real, strings.TrimPrefix(b.TmpDir, "/tmp/"))
		if !slices.Contains(a.Deny, want) {
			t.Errorf("deny %v lacks the real path %s", a.Deny, want)
		}
	}
	// The hashed folder fits under DefaultLayout's root; TmpDir only
	// computes paths. Reason: the test root above is a random-length
	// MkdirTemp name, longer than /tmp/orch-<uid>, so with a 4-digit uid
	// (CI runners) a hashed folder in it can exceed claudeTmpMax.
	def := Layout{Dir: w.layout.Dir, Tmp: DefaultLayout().Tmp}
	long1, long2 := strings.Repeat("x", 32), strings.Repeat("y", 32)
	d1, d2 := def.TmpDir(long1), def.TmpDir(long2)
	if d1 == d2 || !strings.HasPrefix(filepath.Base(d1), "_") || filepath.Dir(d1) != def.Tmp {
		t.Errorf("long names: %s, %s", d1, d2)
	}
	if ClaudeSandboxTmp(d1) == SharedClaudeTmp() {
		t.Errorf("hashed folder %s still too long", d1)
	}
	if def.TmpDir(long1) != d1 {
		t.Error("hashed folder not stable")
	}
	if h := w.layout.TmpDir(long1); !strings.HasPrefix(filepath.Base(h), "_") || filepath.Dir(h) != w.layout.Tmp {
		t.Errorf("long name under the test root: %s", h)
	}
	u := ForUnplaced(w.res, filepath.Join(w.code, "a", "x"), w.layout)
	if !slices.Contains(u.Deny, a.TmpDir) || !slices.Contains(u.Deny, b.TmpDir) {
		t.Errorf("unplaced deny %v lacks the spaces' tmp folders", u.Deny)
	}
}

// TestClaudeSandboxTmp: a short CLAUDE_CODE_TMPDIR gives sandboxed commands
// a folder in it; a long one the shared folder, as measured (RC-184).
func TestClaudeSandboxTmp(t *testing.T) {
	uid := fmt.Sprintf("claude-%d", os.Getuid())
	if got := ClaudeSandboxTmp("/tmp/oe-a"); got != "/tmp/oe-a/"+uid {
		t.Errorf("short: %s", got)
	}
	if got := ClaudeSandboxTmp("/tmp/" + strings.Repeat("l", 40)); got != "/tmp/"+uid || got != SharedClaudeTmp() {
		t.Errorf("long: %s", got)
	}
	if l := (Layout{Dir: "/d"}); l.TmpDir("joba") != "/d/joba/tmp" {
		t.Errorf("no Tmp root: %s", l.TmpDir("joba"))
	}
	if l := DefaultLayout(); l.Tmp != fmt.Sprintf("/tmp/orch-%d", os.Getuid()) || ClaudeSandboxTmp(l.TmpDir("joba")) == SharedClaudeTmp() {
		t.Errorf("default layout %+v", l)
	}
}

// TestLauncherFragment (SP-046.1.a6): a launched session, whose own temp
// folder is elsewhere, is denied the shared folder sessions started without
// the launcher use; not when its folder is too long and it uses the shared
// one itself.
func TestLauncherFragment(t *testing.T) {
	w := newWorld(t)
	w.layout.Tmp = filepath.Join(shortTmp(t), "r")
	f := LauncherFragment(For(w.res, "joba", w.layout))
	shared := SharedClaudeTmp()
	for _, key := range []string{keyDenyRead, keyDenyWrite} {
		if !slices.Contains(f.Lists[key], shared) {
			t.Errorf("%s %v lacks %s", key, f.Lists[key], shared)
		}
	}
	if !slices.Contains(f.Lists[keyPermDeny], "Read(/"+shared+"/**)") {
		t.Errorf("no Read deny of %s: %v", shared, f.Lists[keyPermDeny])
	}
	if real, _ := filepath.EvalSymlinks("/tmp"); real != "/tmp" && !slices.Contains(f.Lists[keyDenyRead], filepath.Join(real, filepath.Base(shared))) {
		t.Errorf("denyRead %v lacks the real path", f.Lists[keyDenyRead])
	}
	if base := ClaudeFragment(For(w.res, "joba", w.layout)); slices.Contains(base.Lists[keyDenyRead], shared) {
		t.Error("the per-checkout fragment denies the shared folder its own session uses")
	}
	long := LauncherFragment(For(w.res, "joba", Layout{Dir: w.layout.Dir}))
	if slices.Contains(long.Lists[keyDenyRead], shared) {
		t.Error("denied the shared folder to a session that uses it")
	}
	if none := LauncherFragment(Rules{}); slices.Contains(none.Lists[keyDenyRead], shared) {
		t.Error("denied the shared folder without a TMPDIR")
	}
}

// TestEnsureTmpDir: the root and the space's folder end up owner-only; a
// root that is a link (another user's, under the shared /tmp) is refused.
func TestEnsureTmpDir(t *testing.T) {
	base := shortTmp(t)
	l := Layout{Dir: filepath.Join(base, "d"), Tmp: filepath.Join(base, "r")}
	if err := l.EnsureTmpDir("joba"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{l.Tmp, l.TmpDir("joba")} {
		if info, err := os.Lstat(d); err != nil || info.Mode().Perm() != 0o700 || !info.IsDir() {
			t.Errorf("%s: %v %v", d, info, err)
		}
	}
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o777); err != nil {
		t.Fatal(err)
	}
	linked := Layout{Dir: l.Dir, Tmp: filepath.Join(base, "link")}
	if err := os.Symlink(other, linked.Tmp); err != nil {
		t.Fatal(err)
	}
	if err := linked.EnsureTmpDir("joba"); err == nil {
		t.Error("a root that is a link was used")
	}
	if _, err := os.Stat(filepath.Join(other, "joba")); !os.IsNotExist(err) {
		t.Errorf("created a folder through the link: %v", err)
	}
	if err := (Layout{Dir: l.Dir}).EnsureTmpDir("joba"); err != nil {
		t.Errorf("no Tmp root: %v", err)
	}
}

// TestRealPath: a path that does not exist yet keeps its missing part below
// its nearest existing parent's real path.
func TestRealPath(t *testing.T) {
	base := realDir(t)
	if err := os.Symlink(base, filepath.Join(base, "ln")); err != nil {
		t.Fatal(err)
	}
	if got, ok := realPath(filepath.Join(base, "ln", "no", "such")); !ok || got != filepath.Join(base, "no", "such") {
		t.Errorf("realPath = %s %v", got, ok)
	}
	if got := withReal([]string{filepath.Join(base, "ln", "x")}); len(got) != 2 || !slices.Contains(got, filepath.Join(base, "x")) {
		t.Errorf("withReal = %v", got)
	}
}

// TestSyncFailsClosedOnTmpRoot (security audit): when the temp root is not
// orch's own (another user's folder, or a link, under the shared /tmp), the
// checkouts get no TMPDIR and no write access to it, and the problem is
// reported; a root too long for Claude Code is reported too.
func TestSyncFailsClosedOnTmpRoot(t *testing.T) {
	w := newWorld(t)
	base := shortTmp(t)
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o777); err != nil {
		t.Fatal(err)
	}
	w.layout.Tmp = filepath.Join(base, "r")
	if err := os.Symlink(other, w.layout.Tmp); err != nil {
		t.Fatal(err)
	}
	s := w.syncer()
	checkout := gitRepo(t, filepath.Join(w.code, "a", "app"))
	rep := s.Sync(context.Background(), w.res, true)
	if !slices.Contains(rep.Written, checkout) || !strings.Contains(strings.Join(rep.Problems, "\n"), "its sessions get none") {
		t.Fatalf("%+v", rep)
	}
	b := readFile(t, localOf(checkout))
	if strings.Contains(b, "TMPDIR") || strings.Contains(b, "allowWrite") {
		t.Errorf("settings still name the temp folder:\n%s", b)
	}
	if !strings.Contains(b, "Read(/"+filepath.Join(w.code, "b")+"/**)") {
		t.Errorf("settings lost their denies:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(other, "joba")); !os.IsNotExist(err) {
		t.Errorf("made a folder through the link: %v", err)
	}

	long := newWorld(t)
	long.layout.Tmp = filepath.Join(shortTmp(t), strings.Repeat("l", 30))
	rep = long.syncer().Sync(context.Background(), long.res, true)
	if !strings.Contains(strings.Join(rep.Problems, "\n"), "too long for Claude Code") {
		t.Errorf("long root not reported: %+v", rep.Problems)
	}
}
