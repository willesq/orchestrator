package handoff

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Checkout is the git working tree a file sits in.
type Checkout struct {
	// Root is the working tree's top directory (a linked worktree's own).
	Root string
	// Owner and Repo name its origin on GitHub; empty when the origin is not
	// a GitHub repository or there is none.
	Owner, Repo string
}

// Bounds on what finding a checkout reads.
const (
	maxGitDepth   = 64
	maxGitPointer = 4 << 10
	maxGitConfig  = 1 << 20
)

// FindCheckout returns the git checkout holding path, by reading the .git
// entries above it, never by running git: a repository's own configuration
// can name programs git would run. Linked worktrees (a .git file pointing
// into another repository's worktrees) resolve to their main repository's
// origin.
func FindCheckout(path string) (Checkout, bool) {
	dir := filepath.Dir(filepath.Clean(path))
	for i := 0; i < maxGitDepth; i++ {
		if gitDir, ok := gitDirAt(dir); ok {
			c := Checkout{Root: dir}
			c.Owner, c.Repo = githubRepo(originURL(commonDir(gitDir)))
			return c, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return Checkout{}, false
}

// gitDirAt is the git directory of a working tree rooted at dir, if any.
func gitDirAt(dir string) (string, bool) {
	p := filepath.Join(dir, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return p, true
	}
	if !info.Mode().IsRegular() {
		return "", false
	}
	b, err := readSmall(p, maxGitPointer)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), true
}

// commonDir is where a git directory's shared config lives: a linked
// worktree's git directory names it in its commondir file.
func commonDir(gitDir string) string {
	b, err := readSmall(filepath.Join(gitDir, "commondir"), maxGitPointer)
	if err != nil {
		return gitDir
	}
	c := strings.TrimSpace(string(b))
	if !filepath.IsAbs(c) {
		c = filepath.Join(gitDir, c)
	}
	return filepath.Clean(c)
}

// originURL reads remote.origin.url from a git directory's config.
func originURL(gitDir string) string {
	b, err := readSmall(filepath.Join(gitDir, "config"), maxGitConfig)
	if err != nil {
		return ""
	}
	in := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = strings.EqualFold(strings.Join(strings.Fields(line), " "), `[remote "origin"]`)
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(k), "url") {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// githubURL matches the forms a GitHub remote takes: scp-like SSH, ssh://
// and https://.
var githubURL = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/|https?://(?:[^@/]+@)?github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// githubRepo parses owner and repo from a GitHub remote URL.
func githubRepo(url string) (owner, repo string) {
	m := githubURL.FindStringSubmatch(url)
	if m == nil || m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." {
		return "", ""
	}
	return m[1], m[2]
}

// readSmall reads at most limit bytes of a regular file. It opens without
// blocking and checks the open file, so a FIFO planted where a .git pointer
// leads cannot stall the reader.
func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
