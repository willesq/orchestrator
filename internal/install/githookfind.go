package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Git hooks orch put in place before it recorded where (githookreg.go) are
// found here, without orchd: in the repositories and folders orch has a
// record of, the global core.hooksPath, and any --scan folder. A hook is
// orch's only when it carries orch's exact marker (IsOrchGitHook); anything
// that merely looks like one is reported, never changed.

// GitRunner runs git with args (an argument array, never a shell line) and
// returns its standard output. Tests inject a fake, so no test runs git
// against a real repository or the real global config.
type GitRunner func(ctx context.Context, args ...string) (string, error)

// GitHookPlaces are where orch may have put git hooks, as far as its own
// records show.
type GitHookPlaces struct {
	// Repos are repositories: registered with `orch repo`, spaces
	// checkouts, trusted test repositories.
	Repos []string
	// Roots hold checkouts a few folders down: known roots, spaces roots,
	// the docs clone cache.
	Roots []string
}

const (
	// rootDepth is how far below a root a checkout is looked for
	// (<root>/<owner>/<repo> and one more).
	rootDepth = 3
	// scanDepth bounds a --scan walk.
	scanDepth = 8
	// maxFindEntries bounds the directory entries one search reads.
	maxFindEntries = 200_000
	// maxGitCalls bounds the repositories git is asked about.
	maxGitCalls = 2000
	// gitCallTimeout bounds one git call.
	gitCallTimeout = 10 * time.Second
)

// skipFindDirs never hold a checkout of their own (spacesettings.Discover
// skips the same).
var skipFindDirs = []string{"node_modules", "vendor", "Library", "__pycache__", "target", "dist", "build"}

// hookFinder collects candidate hooks directories and what it could not
// search.
type hookFinder struct {
	git      GitRunner
	budget   int
	gitCalls int
	found    map[string]string // dir -> where it was found
	order    []string
	notes    []string
}

func (f *hookFinder) add(dir, source string) {
	if !filepath.IsAbs(dir) {
		return
	}
	dir = filepath.Clean(dir)
	if _, ok := f.found[dir]; ok {
		return
	}
	f.found[dir] = source
	f.order = append(f.order, dir)
}

func (f *hookFinder) runGit(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCallTimeout)
	defer cancel()
	return f.git(ctx, args...)
}

// globalHooksPaths adds the core.hooksPath set outside any repository.
// Reason: system config is skipped under a relocated --home, so a test
// home never reaches the machine's own.
func (f *hookFinder) globalHooksPaths(relocated bool) {
	if f.git == nil {
		return
	}
	scopes := []string{"--global"}
	if !relocated {
		scopes = append(scopes, "--system")
	}
	for _, scope := range scopes {
		out, err := f.runGit("config", scope, "--type=path", "--get", "core.hooksPath")
		if err != nil {
			continue // unset
		}
		if dir := strings.TrimSpace(out); dir != "" {
			f.add(dir, scope[2:]+" core.hooksPath")
		}
	}
}

// repo adds a repository's hooks directories: <git dir>/hooks, and the
// one git uses (core.hooksPath when set). A path that is gone or not a
// repository adds nothing.
func (f *hookFinder) repo(path, source string) {
	if !filepath.IsAbs(path) {
		return
	}
	path = filepath.Clean(path)
	gitInfo, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return
	}
	if gitInfo.IsDir() {
		f.add(filepath.Join(path, ".git", "hooks"), source)
	}
	if f.git == nil {
		return
	}
	if f.gitCalls++; f.gitCalls > maxGitCalls {
		if f.gitCalls == maxGitCalls+1 {
			f.notes = append(f.notes, fmt.Sprintf("git was asked about %d repositories and no more; "+
				"search the rest with --scan", maxGitCalls))
		}
		return
	}
	// Reason: the repository's own config is read, so nothing in it may
	// make this call run a program.
	out, err := f.runGit("-c", "core.fsmonitor=false", "-C", path,
		"rev-parse", "--path-format=absolute", "--git-common-dir", "--git-path", "hooks")
	if err != nil {
		f.notes = append(f.notes, "could not ask git for the hooks folder of "+path+"; search it with --scan if orch's hooks may be there")
		return
	}
	// Reason: exactly two absolute paths, or nothing is taken from the
	// answer (a path holding a newline would shift the lines).
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !filepath.IsAbs(lines[0]) || !filepath.IsAbs(lines[1]) {
		f.notes = append(f.notes, "git named no usable hooks folder for "+path+"; search it with --scan if orch's hooks may be there")
		return
	}
	f.add(filepath.Join(lines[0], "hooks"), source)
	f.add(lines[1], source+" (core.hooksPath)")
}

// walk searches root, maxDepth folders down, never following a symlink
// (the root included). In a scan every folder holding a hook is a
// candidate and checkouts are searched through; under a root only
// checkouts are, and a checkout is not searched for others.
func (f *hookFinder) walk(root string, maxDepth int, scan bool, source string) {
	if !filepath.IsAbs(root) {
		f.notes = append(f.notes, root+" is not an absolute path; not searched")
		return
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	switch {
	case err != nil:
		if scan {
			f.notes = append(f.notes, "could not search "+root+": "+err.Error())
		}
		return
	case info.Mode()&os.ModeSymlink != 0:
		f.notes = append(f.notes, root+" is a symlink; not followed (search its target with --scan)")
		return
	case !info.IsDir():
		return
	}
	if !f.visit(root, 0, maxDepth, scan, source) {
		f.notes = append(f.notes, fmt.Sprintf("the search of %s stopped after %d entries; "+
			"search the folders that hold your repositories with --scan", root, maxFindEntries))
	}
}

// visit reads one folder; false when the entry budget ran out.
func (f *hookFinder) visit(dir string, depth, maxDepth int, scan bool, source string) bool {
	if f.budget <= 0 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	f.budget -= len(entries)
	isRepo, holdsHook := false, false
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), GitHookPrevSuffix)
		if slices.Contains(GitHookNames, name) {
			holdsHook = true
		}
		if e.Name() == ".git" {
			isRepo = true
		}
	}
	// Reason: only a folder named hooks is taken for a hooks folder. A
	// copy of orch's hook elsewhere (a dotfiles or template repository)
	// is the user's file, and is reported instead.
	if scan && holdsHook {
		if filepath.Base(dir) == "hooks" {
			f.add(dir, source)
		} else if holdsOrchGitHook(dir) {
			f.notes = append(f.notes, dir+" holds a copy of an orch git hook but is not a hooks folder; left alone")
		}
	}
	if isRepo {
		f.repo(dir, source)
		if !scan && depth > 0 {
			return true
		}
	}
	if depth >= maxDepth {
		return true
	}
	for _, e := range entries {
		// DirEntry types come from lstat: a symlink is never a directory here.
		if !e.IsDir() || e.Name() == ".git" {
			continue
		}
		if !scan && (strings.HasPrefix(e.Name(), ".") || slices.Contains(skipFindDirs, e.Name())) {
			continue
		}
		if !f.visit(filepath.Join(dir, e.Name()), depth+1, maxDepth, scan, source) {
			return false
		}
	}
	return true
}

// findGitHookDirs lists every hooks directory the search found, with
// where each came from, and what it could not search.
func (m Machine) findGitHookDirs() (dirs []string, source map[string]string, notes []string) {
	f := &hookFinder{git: m.Git, budget: maxFindEntries, found: map[string]string{}}
	f.globalHooksPaths(m.Relocated)
	var places GitHookPlaces
	if m.GitHookPlaces != nil {
		places = m.GitHookPlaces()
	}
	for _, r := range places.Repos {
		f.repo(r, "a repository orch has a record of")
	}
	for _, r := range places.Roots {
		f.walk(r, rootDepth, false, "under "+r)
	}
	for _, s := range m.GitHookScan {
		f.walk(s, scanDepth, true, "--scan "+s)
	}
	return f.order, f.found, f.notes
}

// inspectHookDir reports, for one hooks directory, what looks like orch's
// but is not positively so: a link to orch's hook, a hook naming orch
// without its marker, a chained hook left behind with orch's hook gone.
func inspectHookDir(dir string) []string {
	var notes []string
	for _, name := range GitHookNames {
		path := filepath.Join(dir, name)
		prev := path + GitHookPrevSuffix
		content, mode, exists, err := lstatHook(path)
		_, _, prevExists, _ := lstatHook(prev)
		ours := false
		switch {
		case err != nil && exists:
			notes = append(notes, path+": "+err.Error()+"; left alone")
		case err != nil:
			notes = append(notes, err.Error()+"; left alone")
		case exists && mode&os.ModeSymlink != 0:
			if linksToOrchHook(path) {
				notes = append(notes, path+" is a link to an orch git hook ("+string(content)+
					"); links are never removed — remove it by hand")
			}
		case exists && IsOrchGitHook(content):
			ours = true
		case exists && bytes.Contains(content, []byte("orch guard git-hook")):
			notes = append(notes, path+" runs `orch guard git-hook` but has no orch marker; left alone — "+
				"remove it by hand if orch put it there")
		}
		if prevExists && !ours {
			if exists {
				notes = append(notes, prev+" is a hook an orch hook chained, and "+name+
					" is no longer orch's; both left alone")
			} else {
				notes = append(notes, prev+" is a hook an orch hook chained, and orch's hook is gone; "+
					"put it back with: mv "+shQuote(prev)+" "+shQuote(path))
			}
		}
	}
	return notes
}

// linksToOrchHook reports whether the link at path leads to a regular file
// carrying orch's marker. Only reading follows the link.
func linksToOrchHook(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxHookFile {
		return false
	}
	// Reason: non-blocking, and checked again once open, so a link swapped
	// for a FIFO since the Stat cannot hang the uninstall.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if fi, err := file.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(file, maxHookFile))
	return err == nil && IsOrchGitHook(b)
}

// gitHookDiscoveryUnit removes orch's hooks from the directories found
// without a record, putting back any hook they chained, and reports what
// looks like orch's but is not positively so. Install leaves them: it only
// repairs recorded directories.
func (m Machine) gitHookDiscoveryUnit() Unit {
	type result struct {
		dirs   []string
		source map[string]string
		notes  []string
	}
	var cached *result
	// Reason: the search walks folders and runs git, so a plan does it once
	// and every printing and applying of the plan reuses its findings; each
	// step still checks the hooks themselves when it runs.
	find := func() *result {
		if cached == nil {
			r := &result{}
			r.dirs, r.source, r.notes = m.findGitHookDirs()
			cached = r
		}
		return cached
	}
	recorded := func() map[string]bool {
		dirs, _ := RegisteredGitHookDirs(m.DataDir)
		out := map[string]bool{}
		for _, d := range dirs {
			out[filepath.Clean(d)] = true
		}
		return out
	}
	backups := filepath.Join(m.DataDir, "backups", "git-hooks")
	return Unit{
		Name:        "git hooks found without a record",
		InstalledBy: "`orch guard git-hook --install` before orch recorded where it put hooks",
		UndoSteps: func() []Step {
			if m.NoGitHooks {
				return nil
			}
			r, rec := find(), recorded()
			var steps []Step
			for _, dir := range r.dirs {
				if rec[dir] || !isRealDir(dir) || !holdsOrchGitHook(dir) {
					continue
				}
				p, err := PlanGitHooks(dir, "", true)
				if err != nil {
					steps = append(steps, Step{Summary: "git hooks: " + dir,
						Run: func(context.Context) (string, error) { return "", err }})
					continue
				}
				steps = append(steps, Step{
					Summary: "git hooks: remove orch's hooks (putting back any they chained) from " + dir +
						"\n  found: " + r.source[dir],
					Run: func(context.Context) (string, error) {
						// Reason: a folder swapped for a link since the plan is
						// refused rather than followed.
						if !isRealDir(dir) {
							return "", fmt.Errorf("%s is no longer a folder — re-run to plan against current state", dir)
						}
						var out bytes.Buffer
						err := p.Apply(&out, backups)
						return out.String(), err
					}})
			}
			return steps
		},
		Leftovers: func() []string {
			if m.NoGitHooks {
				return nil
			}
			r := find()
			notes := slices.Clone(r.notes)
			rec, _ := RegisteredGitHookDirs(m.DataDir)
			seen := map[string]bool{}
			for _, dir := range append(slices.Clone(r.dirs), rec...) {
				if dir = filepath.Clean(dir); seen[dir] {
					continue
				}
				seen[dir] = true
				info, err := os.Lstat(dir)
				switch {
				case err != nil:
				case info.Mode()&os.ModeSymlink != 0:
					notes = append(notes, "hooks folder "+dir+" is a symlink; not followed (search its target with --scan)")
				case info.IsDir():
					notes = append(notes, inspectHookDir(dir)...)
				}
			}
			if len(m.GitHookScan) == 0 {
				notes = append(notes, "git hooks in repositories orch has no record of (not registered, not a spaces "+
					"checkout, not under a known root) are not searched: `orch uninstall --scan <folder>` finds them")
			}
			return notes
		},
	}
}
