// Package mdwalk finds the markdown files in a repository that are worth
// ingesting: the project's own documentation, not the third-party and generated
// trees checked in or built beside it.
package mdwalk

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultExcludeDirs is the default for the ingest.exclude_dirs setting: directory
// names whose contents are never the project's own docs. Vendored dependencies
// are the costly case. Their READMEs are numerous, often committed (Go's
// vendor/), and generic enough to outrank the project's real knowledge in search.
var DefaultExcludeDirs = []string{
	"node_modules", "bower_components", "vendor", "third_party", "Pods",
	".venv", "venv", "__pycache__", ".tox", ".terraform", ".next",
	"dist", "build", "target",
}

// Files calls fn with the path of every *.md file under root. It never descends
// into a directory whose name is in exclude, nor into .git, which holds no docs
// whatever the configuration says. Names match exactly. The root itself is
// always walked, whatever its name. Unreadable entries are passed over, since
// ingest is best-effort per file.
func Files(root string, exclude []string, fn func(path string)) {
	FilesComplete(root, exclude, fn)
}

// FilesComplete is Files, reporting whether every entry could be read: a
// caller that deletes what the walk did not find must not trust a walk that
// passed over an unreadable directory.
func FilesComplete(root string, exclude []string, fn func(path string)) bool {
	complete := true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			return nil
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || slices.Contains(exclude, d.Name())) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			fn(p)
		}
		return nil
	})
	return complete
}
