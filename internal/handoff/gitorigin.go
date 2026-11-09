package handoff

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"
)

// Checkout is the git working tree a file sits in.
type Checkout struct {
	// Root is the working tree's top directory (a linked worktree's own).
	Root string
	// Main is the main checkout every linked worktree of the repository
	// shares (Root itself when not linked), or the git directory of a bare
	// repository. It is "" for an anonymous checkout.
	Main string
	// Owner and Repo name its origin on GitHub; empty when the origin is not
	// a GitHub repository or there is none. The origin is whatever the
	// repository's config says: callers trust it only when it names a repo
	// they already know.
	Owner, Repo string
	// Branch is the checked-out branch ("" when HEAD is detached or
	// unreadable), and DefaultBranch the repository's default ("" when
	// unknown; see defaultBranch).
	Branch, DefaultBranch string
	// Linked marks a linked worktree (a .git file); Anonymous a .git that does
	// not follow git's layout, whose identity is not believed; Unreadable a
	// HEAD naming no usable branch.
	Linked, Anonymous, Unreadable bool
	// AnonymousWhy says why an anonymous checkout's .git was not believed.
	AnonymousWhy string
	// TrackWhy says why a branch named like the default is not taken as
	// the origin's default branch (see upstreamWhy); "" when it is.
	TrackWhy string
}

// OnDefaultBranch reports whether the checkout is on its default branch.
func (c Checkout) OnDefaultBranch() bool {
	ok, _ := c.DefaultBranchState()
	return ok
}

// DefaultBranchState reports whether the checkout is on its default branch,
// and when not, why, in words for a status line.
func (c Checkout) DefaultBranchState() (bool, string) {
	switch {
	case c.Anonymous:
		if c.AnonymousWhy != "" {
			return false, c.AnonymousWhy
		}
		return false, "its .git does not follow git's layout"
	case c.Unreadable:
		return false, "its HEAD cannot be read (a reftable repository?)"
	case c.Branch == "":
		return false, "HEAD is detached"
	case c.DefaultBranch == "":
		return false, "on branch " + c.Branch + ", and the default branch is unknown (run: git remote set-head origin --auto)"
	case c.Branch != c.DefaultBranch:
		return false, "on branch " + c.Branch + ", not the default branch " + c.DefaultBranch
	case c.TrackWhy != "":
		return false, c.TrackWhy
	}
	return true, ""
}

// Bounds on what finding a checkout reads.
const (
	maxGitDepth   = 64
	maxGitPointer = 4 << 10
	maxGitConfig  = 1 << 20
	maxRefs       = 4096
)

// FindCheckout returns the git checkout holding path, by reading the .git
// entries above it, never by running git: a repository's own configuration
// can name programs git would run. Linked worktrees (a .git file pointing
// into another repository's worktrees) resolve to their main repository's
// origin.
func FindCheckout(path string) (Checkout, bool) {
	dir := filepath.Dir(filepath.Clean(path))
	for i := 0; i < maxGitDepth; i++ {
		if c, ok := CheckoutAt(dir); ok {
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

// CheckoutAt returns the checkout whose root is dir itself, if dir has a
// .git entry; it never looks above dir.
func CheckoutAt(dir string) (Checkout, bool) {
	dir = filepath.Clean(dir)
	gitDir, linked, found, why := gitDirAt(dir)
	if !found {
		return Checkout{}, false
	}
	c := Checkout{Root: dir, Linked: linked}
	if gitDir == "" {
		// A .git file that does not point back here could borrow any
		// repository's identity; the checkout is anonymous.
		c.Anonymous, c.AnonymousWhy = true, why
		return c, true
	}
	common, ok := commonDir(gitDir)
	if !ok {
		c.Anonymous = true
		return c, true
	}
	c.Main = common
	if filepath.Base(common) == ".git" {
		c.Main = filepath.Dir(common)
	}
	c.Owner, c.Repo = githubRepo(originURL(common))
	head, readable := headRef(gitDir)
	c.Branch, c.Unreadable = head, !readable
	c.DefaultBranch = defaultBranch(common)
	if c.Branch != "" && c.Branch == c.DefaultBranch {
		c.TrackWhy = upstreamWhy(common, c.Branch)
	}
	return c, true
}

// gitDirAt finds the git directory of a working tree rooted at dir: found
// reports a .git entry at all. A .git directory is the git directory. A .git
// file (its path absolute or relative to dir) is believed when it points, with
// symlinks resolved, at <common>/worktrees/<name> whose gitdir file points
// back at this .git file (absolute, or relative to that directory, as
// `git worktree add --relative-paths` writes), or at a git directory whose
// core.worktree names dir. Anything else, a `--separate-git-dir` repository
// included, is found but anonymous (gitDir ""), with why.
func gitDirAt(dir string) (gitDir string, linked, found bool, why string) {
	p := filepath.Join(dir, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return "", false, false, ""
	}
	if info.IsDir() {
		return p, false, true, ""
	}
	if !info.Mode().IsRegular() {
		return "", false, false, ""
	}
	bad := func(reason string) (string, bool, bool, string) { return "", true, true, reason }
	b, err := readSmall(p, maxGitPointer)
	if err != nil {
		return bad("its .git file cannot be read")
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return bad("its .git file is not a gitdir pointer")
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(target))
	if err != nil {
		return bad("its .git points at a git directory that does not exist")
	}
	// git keeps a linked worktree's or submodule's git directory outside it.
	// One inside the tree came with the files (an archive or a copy), so
	// its refs and back-pointer say nothing about where the files came from.
	if within(real, dir) {
		return bad("its .git points at a git directory inside the checkout itself")
	}
	// A submodule, or a --separate-git-dir repository with core.worktree set:
	// its git directory names this working tree.
	if wt := configValue(real, "core", "worktree"); wt != "" {
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(real, wt)
		}
		if samePath(wt, dir) {
			return real, false, true, ""
		}
	}
	if filepath.Base(filepath.Dir(real)) != "worktrees" {
		return bad("its .git points at a git directory elsewhere (a --separate-git-dir repository?); " +
			"set core.worktree there to this directory to index it")
	}
	back, err := readSmall(filepath.Join(real, "gitdir"), maxGitPointer)
	if err != nil {
		return bad("its worktree's gitdir file cannot be read")
	}
	bp := strings.TrimSpace(string(back))
	if !filepath.IsAbs(bp) {
		bp = filepath.Join(real, bp)
	}
	if !samePath(bp, p) {
		return bad("its worktree's gitdir file points at another working tree")
	}
	return real, true, true, ""
}

// within reports whether path p is dir or below it, comparing cleaned paths
// with symlinks resolved on both sides, at a separator boundary: /a/proj.git
// and /a/proj2 are not within /a/proj. Each existing ancestor of p is also
// compared with dir as a file, so a path spelled in another case on a
// case-insensitive volume is still inside.
func within(p, dir string) bool {
	resolve := func(x string) string {
		if r, err := filepath.EvalSymlinks(x); err == nil {
			return r
		}
		return filepath.Clean(x)
	}
	rp, rd := resolve(p), resolve(dir)
	if rp == rd || strings.HasPrefix(rp, strings.TrimSuffix(rd, string(filepath.Separator))+string(filepath.Separator)) {
		return true
	}
	di, err := os.Stat(rd)
	if err != nil {
		return false
	}
	for a := rp; ; a = filepath.Dir(a) {
		if ai, err := os.Stat(a); err == nil && os.SameFile(ai, di) {
			return true
		}
		if filepath.Dir(a) == a {
			return false
		}
	}
}

// samePath compares two paths after cleaning and resolving symlinks.
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(filepath.Clean(a))
	rb, err2 := filepath.EvalSymlinks(filepath.Clean(b))
	return err1 == nil && err2 == nil && ra == rb
}

// commonDir is where a git directory's shared config lives: a linked
// worktree's git directory names it in its commondir file. Git lays linked
// worktrees out as <common>/worktrees/<name>, so a commondir naming anything
// else is refused (ok false) rather than followed.
func commonDir(gitDir string) (string, bool) {
	b, err := readSmall(filepath.Join(gitDir, "commondir"), maxGitPointer)
	if err != nil {
		return gitDir, true
	}
	c := strings.TrimSpace(string(b))
	if !filepath.IsAbs(c) {
		c = filepath.Join(gitDir, c)
	}
	c = filepath.Clean(c)
	if real, err := filepath.EvalSymlinks(c); err == nil {
		c = real
	}
	if filepath.Dir(filepath.Clean(gitDir)) != filepath.Join(c, "worktrees") {
		return "", false
	}
	return c, true
}

// headRef is the branch HEAD names: "" when detached; readable false when
// HEAD is missing or names no usable branch (a reftable repository writes
// "refs/heads/.invalid" there).
func headRef(gitDir string) (string, bool) {
	b, err := readSmall(filepath.Join(gitDir, "HEAD"), maxGitPointer)
	if err != nil {
		return "", false
	}
	t := strings.TrimSpace(string(b))
	ref, ok := strings.CutPrefix(t, "ref:")
	if !ok {
		return "", true // a commit id: detached
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/")
	if !ok || !validRefName(name) {
		return "", false
	}
	return name, true
}

// headBranch is the branch a git directory's HEAD names, "" when detached
// or unreadable.
func headBranch(gitDir string) string {
	b, _ := headRef(gitDir)
	return b
}

// defaultBranch is the repository's default branch, in order: the origin's
// HEAD as last fetched; init.defaultBranch from the repository's config, when
// such a branch exists locally; main or master, when one exists. "" when none
// of these settles it. A lone local branch is not taken as the default: a
// single-branch or PR clone has exactly one.
func defaultBranch(common string) string {
	if b := symref(filepath.Join(common, "refs", "remotes", "origin", "HEAD"), "refs/remotes/origin/"); b != "" {
		return b
	}
	local := localBranches(common)
	if b := configValue(common, "init", "defaultbranch"); b != "" && local[b] {
		return b
	}
	for _, b := range []string{"main", "master"} {
		if local[b] {
			return b
		}
	}
	return ""
}

// localBranches lists branches from loose refs and packed-refs.
func localBranches(common string) map[string]bool {
	out := map[string]bool{}
	heads := filepath.Join(common, "refs", "heads")
	_ = filepath.WalkDir(heads, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(out) >= maxRefs {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if rel, err := filepath.Rel(heads, p); err == nil {
				out[filepath.ToSlash(rel)] = true
			}
		}
		return nil
	})
	if b, err := readSmall(filepath.Join(common, "packed-refs"), maxGitConfig); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && len(out) < maxRefs {
				if name, ok := strings.CutPrefix(f[1], "refs/heads/"); ok {
					out[name] = true
				}
			}
		}
	}
	return out
}

// symref reads a symbolic ref file ("ref: <prefix><name>") and returns name.
func symref(path, prefix string) string {
	b, err := readSmall(path, maxGitPointer)
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref:")
	if !ok {
		return ""
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(ref), prefix)
	if !ok || !validRefName(name) {
		return ""
	}
	return name
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

// badRefRune reports a rune git never allows in a branch name: whitespace,
// control and format characters. HEAD is the repository's to write, so a
// name that breaks this is not used (it ends up in status lines).
// validRefName reports whether a branch name read from a ref file is one git
// could have written and a status line can show: no whitespace, control or
// format characters, none of ~^:, at most 255 bytes, and not the
// placeholder a reftable repository leaves.
func validRefName(name string) bool {
	if name == "" || name == ".invalid" || len(name) > 255 || strings.IndexFunc(name, badRefRune) >= 0 {
		return false
	}
	// git check-ref-format's path rules, so a name can never lead a ref
	// lookup out of the git directory.
	for _, bad := range []string{"..", "//", "@{", "\\"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	return !strings.HasPrefix(name, "/") && !strings.HasSuffix(name, "/") && !strings.HasSuffix(name, ".lock") &&
		!strings.HasPrefix(name, ".") && !strings.Contains(name, "/.")
}

func badRefRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '~' || r == '^' || r == ':'
}
