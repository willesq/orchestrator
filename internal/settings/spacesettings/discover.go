package spacesettings

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Discovery bounds. Reason: orchd walks every root on each sync, and a root
// such as ~/code can hold large trees that are not checkouts.
const (
	// MaxDepth is how many folders below a root a checkout may sit.
	MaxDepth = 6
	// maxVisited caps the folders one discovery looks at.
	maxVisited = 50_000
)

// skipDirs are folders that never hold a checkout of their own.
var skipDirs = []string{"node_modules", "vendor", "Library", "__pycache__", "target", "dist", "build"}

// Discover returns every git checkout (a folder holding a .git folder or
// file) in or under roots, at most MaxDepth folders down, sorted. Below a
// root it does not look inside a checkout for another, follow links, or
// enter hidden folders. truncated reports that the folder cap stopped the
// walk early.
func Discover(roots []string) (checkouts []string, truncated bool) {
	seen := map[string]bool{}
	visited := 0
	for _, root := range roots {
		root = filepath.Clean(root)
		base := strings.Count(root, string(filepath.Separator))
		// The trailing separator makes the walk follow a root that is
		// itself a link, as placement does; links below it are not followed.
		_ = filepath.WalkDir(root+string(filepath.Separator), func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			path = filepath.Clean(path)
			if visited++; visited > maxVisited {
				truncated = true
				return filepath.SkipAll
			}
			if path != root && (strings.HasPrefix(d.Name(), ".") || slices.Contains(skipDirs, d.Name())) {
				return filepath.SkipDir
			}
			if hasGit(path) {
				if !seen[path] {
					seen[path] = true
					checkouts = append(checkouts, path)
				}
				// Reason: a root that is itself a checkout (a home folder
				// kept in git, say) still holds the user's other checkouts.
				if path != root {
					return filepath.SkipDir
				}
			}
			if strings.Count(path, string(filepath.Separator))-base >= MaxDepth {
				return filepath.SkipDir
			}
			return nil
		})
	}
	slices.Sort(checkouts)
	return checkouts, truncated
}

// hasGit reports whether dir holds a .git folder or file (a worktree's).
func hasGit(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular()
}
