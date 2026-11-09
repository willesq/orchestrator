package spaces

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxSymlinks bounds the links followed while resolving one path, as the
// kernel's ELOOP limit does.
const maxSymlinks = 40

// maxGitFile bounds what is read of a worktree's .git file and commondir.
const maxGitFile = 4096

// pathResolver resolves paths the way the kernel does and maps linked git
// worktrees to their main checkout. It caches per directory, so one tool
// call touching many files in a few directories costs a few lstats each.
type pathResolver struct {
	real      map[string]string
	worktrees map[string]string
}

func newPathResolver() *pathResolver {
	return &pathResolver{real: map[string]string{}, worktrees: map[string]string{}}
}

// resolution is a path as the kernel resolves it. existing is its deepest
// directory or file that exists; missing is set when part of it does not
// exist; broken when a link could not be followed.
type resolution struct {
	path     string
	existing string
	missing  bool
	broken   bool
}

// resolve returns the physical path p names, resolving symlinks one
// component at a time from the left. p must be absolute and is not cleaned
// first: "link/.." is the parent of the link's target, not the directory
// holding the link, so cleaning first would let a link hide a target.
// Components after the first that does not exist are joined lexically.
func (r *pathResolver) resolve(p string) resolution {
	if p == "" || p[0] != '/' {
		return resolution{path: filepath.Clean(p), missing: true}
	}
	cur := "/"
	rest := p
	links := 0
	for rest != "" {
		var comp string
		comp, rest = cutComponent(rest)
		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, comp)
		if v, ok := r.real[next]; ok {
			cur = v
			continue
		}
		fi, err := os.Lstat(next)
		if err != nil {
			return resolution{path: filepath.Join(cur, comp, rest), existing: cur, missing: true}
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			r.real[next] = next
			cur = next
			continue
		}
		links++
		target, err := os.Readlink(next)
		if err != nil || links > maxSymlinks {
			return resolution{path: filepath.Join(cur, comp, rest), existing: cur, missing: true, broken: true}
		}
		if strings.HasPrefix(target, "/") {
			cur = "/"
		}
		rest = target + "/" + rest
	}
	return resolution{path: cur, existing: cur}
}

func cutComponent(s string) (string, string) {
	s = strings.TrimLeft(s, "/")
	comp, rest, _ := strings.Cut(s, "/")
	return comp, rest
}

// mainCheckout returns the path p corresponds to in its main checkout when
// p is inside a linked git worktree, and false otherwise. p must already be
// resolved. The worktree's .git file names its git dir, whose commondir is
// the main checkout's .git directory.
func (r *pathResolver) mainCheckout(p, existing string) (string, bool) {
	if existing == "" {
		return "", false
	}
	for dir := existing; ; dir = filepath.Dir(dir) {
		main, found := r.worktreeAt(dir)
		if found {
			if main == "" {
				return "", false
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return "", false
			}
			return filepath.Join(main, rel), true
		}
		if dir == "/" {
			return "", false
		}
	}
}

// worktreeAt reports whether dir holds a .git entry and, when it is a
// linked worktree of a non-bare repo, the main checkout's path.
func (r *pathResolver) worktreeAt(dir string) (main string, found bool) {
	if v, ok := r.worktrees[dir]; ok {
		return v, v != "\x00"
	}
	main, found = r.readWorktree(dir)
	if !found {
		r.worktrees[dir] = "\x00"
	} else {
		r.worktrees[dir] = main
	}
	return main, found
}

func (r *pathResolver) readWorktree(dir string) (string, bool) {
	dotgit := filepath.Join(dir, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return "", false
	}
	if !fi.Mode().IsRegular() {
		return "", true
	}
	data, err := readSmall(dotgit, maxGitFile)
	if err != nil {
		return "", true
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "gitdir:")
	if !ok {
		return "", true
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	common, err := readSmall(filepath.Join(gitdir, "commondir"), maxGitFile)
	if err != nil {
		return "", true
	}
	cd := strings.TrimSpace(string(common))
	if !filepath.IsAbs(cd) {
		cd = filepath.Join(gitdir, cd)
	}
	cd = r.resolve(cd).path
	if filepath.Base(cd) != ".git" {
		return "", true
	}
	return filepath.Dir(cd), true
}

// readSmall reads at most limit bytes of a regular file. It opens without
// blocking and checks the type of what it opened, so a FIFO or device
// planted where a file is expected cannot hang the hook.
func readSmall(path string, limit int64) ([]byte, error) {
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

var errNotRegular = errors.New("not a regular file")
