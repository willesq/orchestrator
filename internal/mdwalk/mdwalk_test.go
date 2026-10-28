package mdwalk

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree creates files (relative paths) under a fresh root named name.
func tree(t *testing.T, name string, files ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	for _, rel := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func collect(root string, exclude []string) []string {
	var got []string
	Files(root, exclude, func(p string) {
		rel, _ := filepath.Rel(root, p)
		got = append(got, rel)
	})
	slices.Sort(got)
	return got
}

func TestFiles(t *testing.T) {
	files := []string{
		"README.md",
		"docs/guide.MD",
		"docs/notes.txt",
		"docs/vendor-guide.md", // only whole directory names match
		"internal/pkg/DESIGN.md",
		"vendor/github.com/charmbracelet/lipgloss/README.md",
		"web/node_modules/x/README.md",
		"third_party/lib/README.md",
		".venv/lib/site-packages/pkg/README.md",
		".git/description.md",
	}
	tests := []struct {
		name    string
		root    string
		exclude []string
		want    []string
	}{
		{
			name: "defaults skip third-party and generated trees",
			root: "repo", exclude: DefaultExcludeDirs,
			want: []string{"README.md", "docs/guide.MD", "docs/vendor-guide.md", "internal/pkg/DESIGN.md"},
		},
		{
			name: "a configured list replaces the defaults",
			root: "repo", exclude: []string{"docs"},
			want: []string{
				".venv/lib/site-packages/pkg/README.md", "README.md", "internal/pkg/DESIGN.md",
				"third_party/lib/README.md", "vendor/github.com/charmbracelet/lipgloss/README.md", "web/node_modules/x/README.md",
			},
		},
		{
			name: "an empty list still never reads .git",
			root: "repo", exclude: nil,
			want: []string{
				".venv/lib/site-packages/pkg/README.md", "README.md", "docs/guide.MD", "docs/vendor-guide.md",
				"internal/pkg/DESIGN.md", "third_party/lib/README.md", "vendor/github.com/charmbracelet/lipgloss/README.md",
				"web/node_modules/x/README.md",
			},
		},
		{
			name: "a root named like an excluded dir is still walked",
			root: "build", exclude: DefaultExcludeDirs,
			want: []string{"README.md", "docs/guide.MD", "docs/vendor-guide.md", "internal/pkg/DESIGN.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tree(t, tt.root, files...)
			if got := collect(root, tt.exclude); !slices.Equal(got, tt.want) {
				t.Errorf("Files =\n  %v\nwant\n  %v", got, tt.want)
			}
		})
	}
}

func TestFilesMissingRoot(t *testing.T) {
	called := false
	Files(filepath.Join(t.TempDir(), "gone"), DefaultExcludeDirs, func(string) { called = true })
	if called {
		t.Errorf("callback ran for a missing root")
	}
}

func TestFilesCompleteReportsUnreadableDirs(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !FilesComplete(root, nil, func(string) {}) {
		t.Fatal("a readable tree reported incomplete")
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	if FilesComplete(root, nil, func(string) {}) {
		t.Fatal("an unreadable directory went unreported")
	}
}

func TestOwnFilesSkipsOtherCheckouts(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"README.md", "docs/a.md", "nested/.git/HEAD", "nested/N.md", "x.worktrees/feat/W.md", "wt/.git", "wt/T.md"} {
		p := filepath.Join(root, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	var got []string
	if !OwnFiles(root, nil, func(p string) { rel, _ := filepath.Rel(root, p); got = append(got, filepath.ToSlash(rel)) }) {
		t.Fatal("incomplete")
	}
	if strings.Join(got, ",") != "README.md,docs/a.md" {
		t.Fatalf("walked %v", got)
	}
	var all []string
	FilesComplete(root, nil, func(p string) { all = append(all, p) })
	if len(all) != 5 {
		t.Fatalf("FilesComplete walked %d", len(all))
	}
	if IsWorktreesDir(".worktrees") || !IsWorktreesDir("repo.worktrees") {
		t.Fatal("IsWorktreesDir")
	}
}

func TestSymlinksOutOfTheTreeAreNotFollowed(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("TOP SECRET"), 0o600)
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "real.md"), []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "notes.md")); err != nil {
		t.Skip(err)
	}
	_ = os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "alias.md"))
	_ = os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling.md"))
	// Inside the tree, but not markdown, or where the walk never goes.
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=x"), 0o600)
	_ = os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "env.md"))
	for _, dir := range []string{".git", "node_modules", "nested/.git"} {
		_ = os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	_ = os.WriteFile(filepath.Join(root, ".git", "config.md"), []byte("url"), 0o600)
	_ = os.Symlink(filepath.Join(root, ".git", "config.md"), filepath.Join(root, "gitcfg.md"))
	_ = os.WriteFile(filepath.Join(root, "node_modules", "pkg.md"), []byte("x"), 0o600)
	_ = os.Symlink(filepath.Join(root, "node_modules", "pkg.md"), filepath.Join(root, "dep.md"))
	_ = os.WriteFile(filepath.Join(root, "nested", "other.md"), []byte("x"), 0o600)
	_ = os.Symlink(filepath.Join(root, "nested", "other.md"), filepath.Join(root, "other-link.md"))
	for name, c := range map[string]struct {
		walk func(string, []string, func(string)) bool
		want string
	}{
		// FilesComplete walks into nested checkouts, so a link to one is fine.
		"FilesComplete": {FilesComplete, "alias.md,other.md,other-link.md,real.md"},
		"OwnFiles":      {OwnFiles, "alias.md,real.md"},
	} {
		var got []string
		c.walk(root, []string{"node_modules"}, func(p string) { got = append(got, filepath.Base(p)) })
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s walked %v, want %s", name, got, c.want)
		}
	}
}
