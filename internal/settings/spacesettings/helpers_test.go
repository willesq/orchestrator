package spacesettings

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// world is a temp home with a code folder holding one root per space:
// code (public oss), code/me (private personal), code/a, code/b and code/c
// (isolated jobs; joba reads personal and has a gh profile).
type world struct {
	home, code string
	res        *spaces.Resolved
	layout     Layout
}

func newWorld(t *testing.T) world {
	t.Helper()
	// The user's git config (a global excludes file) must not decide what
	// the tests see as ignored.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	home := realDir(t)
	// git reads $XDG_CONFIG_HOME/git/ignore even without a global config.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("HOME", home)
	w := world{home: home, code: filepath.Join(home, "code"), layout: Layout{Dir: filepath.Join(home, "data", "spaces")}}
	for _, d := range []string{"code/me", "code/a", "code/b", "code/c", "gh/a"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "oss", Kind: spaces.Public, Roots: []string{"~/code"}},
		{Name: "personal", Kind: spaces.Private, Roots: []string{"~/code/me"}},
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}, Reads: []string{"personal"}, GHConfigDir: "~/gh/a"},
		{Name: "jobb", Kind: spaces.Isolated, Roots: []string{"~/code/b"}},
		{Name: "jobc", Kind: spaces.Isolated, Roots: []string{"~/code/c"}},
	}}
	res, err := spaces.Validate(p, spaces.Env{Home: home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	w.res = res
	return w
}

// realDir is a temp folder by its real path (macOS's temp folder is behind
// a link), so expected paths match what the code resolves.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// placeByRoot places a checkout by its deepest root only.
func (w world) placeByRoot(_ context.Context, checkout string) (string, bool, error) {
	space, best := "", -1
	for _, r := range w.res.BoundRoots() {
		if spaces.RootHolds(r.Root, checkout) && len(r.Root) > best {
			space, best = r.Space, len(r.Root)
		}
	}
	return space, space != "", nil
}

func (w world) syncer() *Syncer {
	return &Syncer{
		Layout:    w.layout,
		StateFile: filepath.Join(w.home, "data", "checkouts.json"),
		BackupDir: filepath.Join(w.home, "data", "backups"),
		Place:     w.placeByRoot,
	}
}

// gitRepo makes a git checkout at dir.
func gitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
