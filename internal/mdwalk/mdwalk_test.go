package mdwalk

import (
	"os"
	"path/filepath"
	"slices"
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
