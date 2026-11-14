package spacesettings

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// TestGeneratedSettingsDenyOtherJobsOnly (SP-046.1.a1): a checkout in job A
// denies every other isolated root, nothing in A, the public space or the
// private space A reads, and sets GH_CONFIG_DIR to the gh profile A names
// (D-59). CLAUDE_CONFIG_DIR is never in a checkout's settings: only the
// launcher can choose Claude Code's folder.
func TestGeneratedSettingsDenyOtherJobsOnly(t *testing.T) {
	w := newWorld(t)
	a, b, c := filepath.Join(w.code, "a"), filepath.Join(w.code, "b"), filepath.Join(w.code, "c")
	r := For(w.res, "joba", w.layout)

	for _, other := range []string{b, c} {
		if !slices.Contains(r.Deny, other) {
			t.Errorf("deny %v lacks other job root %s", r.Deny, other)
		}
	}
	// Nothing of A, of the public root or of personal (which A reads) is
	// denied: no deny is at or above them, and none is below them except
	// other jobs' roots nested in the public root.
	for _, mine := range []string{a, w.code, filepath.Join(w.code, "me"), w.layout.SpaceDir("joba")} {
		for _, d := range r.Deny {
			if spaces.RootHolds(d, mine) {
				t.Errorf("deny %s covers %s", d, mine)
			}
			if spaces.RootHolds(mine, d) && d != b && d != c {
				t.Errorf("deny %s lies in %s", d, mine)
			}
		}
	}
	if want := filepath.Join(w.home, "gh", "a"); r.GHConfigDir != want {
		t.Errorf("GHConfigDir = %q, want %q", r.GHConfigDir, want)
	}

	f := ClaudeFragment(r)
	if f.Scalars["env.GH_CONFIG_DIR"] != filepath.Join(w.home, "gh", "a") {
		t.Errorf("env.GH_CONFIG_DIR = %v", f.Scalars["env.GH_CONFIG_DIR"])
	}
	if _, ok := f.Scalars["env.CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("checkout settings set CLAUDE_CONFIG_DIR: %v", f.Scalars)
	}
	if f.Scalars[keySandboxOn] != true || f.Scalars[keyUnsandboxed] != false || f.Scalars[keyAllProjectMCP] != false {
		t.Errorf("sandbox scalars = %v", f.Scalars)
	}
	// The hooks stay on and find orch's data folder whatever a project
	// file says (D-67); with no data folder in the layout none is pinned.
	if f.Scalars[keyNoHooks] != false {
		t.Errorf("disableAllHooks = %v", f.Scalars[keyNoHooks])
	}
	if _, ok := f.Scalars["env."+envOrchData]; ok {
		t.Errorf("ORCH_DATA_DIR pinned without a data folder: %v", f.Scalars)
	}
	l := w.layout
	l.DataDir = filepath.Join(w.home, "data")
	if got := ClaudeFragment(For(w.res, "joba", l)).Scalars["env."+envOrchData]; got != l.DataDir {
		t.Errorf("env.ORCH_DATA_DIR = %v, want %s", got, l.DataDir)
	}
	if got := ClaudeFragment(ForUnplaced(w.res, filepath.Join(w.code, "x"), l)).Scalars["env."+envOrchData]; got != l.DataDir {
		t.Errorf("unplaced env.ORCH_DATA_DIR = %v", got)
	}
	for _, want := range []string{"Read(/" + b + "/**)", "Edit(/" + b + "/**)", "Read(/" + c + "/**)"} {
		if !slices.Contains(f.Lists[keyPermDeny], want) {
			t.Errorf("permissions.deny %v lacks %s", f.Lists[keyPermDeny], want)
		}
	}
	for _, key := range []string{keyDenyRead, keyDenyWrite} {
		if !slices.Equal(f.Lists[key], r.Deny) {
			t.Errorf("%s = %v, want %v", key, f.Lists[key], r.Deny)
		}
	}
	for _, rule := range f.Lists[keyPermDeny] {
		if strings.Contains(rule, "/code/a") || strings.Contains(rule, "/code/me") {
			t.Errorf("rule %s denies A or what A reads", rule)
		}
	}
}

// TestOtherProfilesAndOrchDirDenied: every space is denied other spaces'
// gh profiles and Claude Code folders and orchd's own folder, never its own.
func TestOtherProfilesAndOrchDirDenied(t *testing.T) {
	w := newWorld(t)
	gh, cl := filepath.Join(w.home, "gh", "a"), filepath.Join(w.home, "claude", "a")
	for _, space := range []string{"jobb", "oss", "personal"} {
		r := For(w.res, space, w.layout)
		if !slices.Contains(r.Deny, gh) || !slices.Contains(r.Deny, cl) || !slices.Contains(r.Deny, w.layout.OrchDir()) {
			t.Errorf("%s deny %v lacks joba's gh profile, Claude folder or orch's folder", space, r.Deny)
		}
	}
	a := For(w.res, "joba", w.layout)
	if slices.Contains(a.Deny, gh) || slices.Contains(a.Deny, cl) || !slices.Contains(a.Deny, w.layout.OrchDir()) {
		t.Errorf("joba deny %v", a.Deny)
	}
	if a.ClaudeConfigDir != cl || For(w.res, "jobb", w.layout).ClaudeConfigDir != "" {
		t.Errorf("ClaudeConfigDir = %q", a.ClaudeConfigDir)
	}
	u := ForUnplaced(w.res, filepath.Join(w.code, "a", "x"), w.layout)
	if !slices.Contains(u.Deny, gh) || !slices.Contains(u.Deny, cl) || !slices.Contains(u.Deny, w.layout.OrchDir()) || u.ClaudeConfigDir != "" {
		t.Errorf("unplaced deny %v", u.Deny)
	}
}

// TestGeneratedSettingsOtherSpaces (SP-046.1.a1, D-59): a public checkout
// denies every job's root; a space that names no gh profile (oss, jobb)
// sets no GH_CONFIG_DIR, so its sessions use the default gh login.
func TestGeneratedSettingsOtherSpaces(t *testing.T) {
	w := newWorld(t)
	r := For(w.res, "oss", w.layout)
	for _, job := range []string{"a", "b", "c"} {
		if !slices.Contains(r.Deny, filepath.Join(w.code, job)) {
			t.Errorf("public deny %v lacks job %s", r.Deny, job)
		}
	}
	if slices.Contains(r.Deny, filepath.Join(w.code, "me")) {
		t.Errorf("public deny %v holds a private root; only isolated roots are denied", r.Deny)
	}
	for _, space := range []string{"oss", "jobb"} {
		r := For(w.res, space, w.layout)
		if _, ok := r.Env()[envGHConfigDir]; ok || r.GHConfigDir != "" {
			t.Errorf("%s env %v sets a gh profile the space does not have", space, r.Env())
		}
		f := CheckoutFragment(r, filepath.Join(w.code, "x"))
		if _, ok := f.Scalars["env."+envGHConfigDir]; ok {
			t.Errorf("%s checkout settings set GH_CONFIG_DIR: %v", space, f.Scalars)
		}
		if f.Scalars["env."+envTmpDir] != w.layout.TmpDir(space) {
			t.Errorf("%s checkout settings lack its TMPDIR: %v", space, f.Scalars)
		}
	}
}

// TestPerSpaceTmpDir (SP-046.1.a6): each space has its own TMPDIR, which
// Claude Code also uses for sandboxed commands, may write it, and is denied
// every other space's directory, temp folder included.
func TestPerSpaceTmpDir(t *testing.T) {
	w := newWorld(t)
	a, b := For(w.res, "joba", w.layout), For(w.res, "jobb", w.layout)
	if a.TmpDir == b.TmpDir || a.TmpDir != w.layout.TmpDir("joba") {
		t.Fatalf("tmp dirs %q and %q", a.TmpDir, b.TmpDir)
	}
	env := a.Env()
	if env["TMPDIR"] != a.TmpDir || env["CLAUDE_CODE_TMPDIR"] != a.TmpDir {
		t.Errorf("env = %v", env)
	}
	if !denies(a.Deny, b.TmpDir) || !denies(b.Deny, a.TmpDir) {
		t.Errorf("a deny %v / b deny %v do not cover each other's temp dirs", a.Deny, b.Deny)
	}
	if denies(a.Deny, a.TmpDir) {
		t.Errorf("a denies its own temp dir")
	}
	f := ClaudeFragment(a)
	if !slices.Equal(f.Lists[keyAllowWrite], []string{a.TmpDir}) {
		t.Errorf("allowWrite = %v", f.Lists[keyAllowWrite])
	}
	if !slices.Contains(f.Lists[keyPermDeny], "Read(/"+w.layout.SpaceDir("jobb")+"/**)") {
		t.Errorf("no Read deny for jobb's directory: %v", f.Lists[keyPermDeny])
	}
}

func denies(deny []string, path string) bool {
	for _, d := range deny {
		if spaces.RootHolds(d, path) {
			return true
		}
	}
	return false
}

// TestUnplacedCheckout: a checkout with no space denies every job's root
// but the one it sits in, and every space's directory, and gets no
// profile or temp dir.
func TestUnplacedCheckout(t *testing.T) {
	w := newWorld(t)
	in := filepath.Join(w.code, "a", "repo")
	r := ForUnplaced(w.res, in, w.layout)
	if denies(r.Deny, in) {
		t.Errorf("deny %v covers the checkout itself", r.Deny)
	}
	if !denies(r.Deny, filepath.Join(w.code, "b")) || !denies(r.Deny, w.layout.SpaceDir("joba")) {
		t.Errorf("deny %v", r.Deny)
	}
	if r.Space != "" || r.TmpDir != "" || r.GHConfigDir != "" || len(r.Env()) != 0 {
		t.Errorf("unplaced rules %+v", r)
	}
}

// TestDenyAddsRealPath: a root reached through a link is denied by both
// paths, and a path holding a glob character gets no permission rule.
func TestDenyAddsRealPath(t *testing.T) {
	dir := realDir(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got := withReal([]string{link, link + "/", filepath.Join(dir, "x[1]")})
	want := []string{link, real, filepath.Join(dir, "x[1]")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("withReal = %v, want %v", got, want)
	}
	f := ClaudeFragment(Rules{Deny: got})
	for _, rule := range f.Lists[keyPermDeny] {
		if strings.Contains(rule, "x[1]") {
			t.Errorf("rule %s names a glob path", rule)
		}
	}
	if !slices.Contains(f.Lists[keyDenyRead], filepath.Join(dir, "x[1]")) {
		t.Errorf("sandbox deny lacks the glob path")
	}
	if len(ClaudeFragment(Rules{}).Lists) != 0 {
		t.Errorf("empty rules give lists")
	}
}

// TestNoPolicyLookups: without a policy the lookups are empty.
func TestNoPolicyLookups(t *testing.T) {
	var res *spaces.Resolved
	if res.BoundRoots() != nil || res.Names() != nil || res.GHConfigDir("x") != "" || res.ClaudeConfigDir("x") != "" {
		t.Error("nil policy has lookups")
	}
	if l := DefaultLayout(); !strings.HasSuffix(l.Dir, "spaces") || l.SpaceDir("x") != filepath.Join(l.Dir, "x") {
		t.Errorf("layout %+v", l)
	}
}
