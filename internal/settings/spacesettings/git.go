package spacesettings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// excludeLine is what orch adds to a checkout's .git/info/exclude when no
// rule ignores the settings file already: the file at the checkout's root.
const excludeLine = "/" + LocalPath

// gitTimeout bounds one git call, so a hung checkout cannot stall the sync.
const gitTimeout = 10 * time.Second

// ErrTracked is returned for a checkout whose settings file is committed:
// writing it would change a tracked file the next commit could publish.
var ErrTracked = errors.New("its .claude/settings.local.json is tracked by git; orch does not write a tracked file")

// Git runs git for the ignore checks.
type Git struct {
	// Bin is the git binary; "" finds git on PATH.
	Bin string
}

// run runs git in checkout and returns its exit code and output. Reason
// for the -c: the checkout's own .git/config is writable by the session
// working in it, so nothing in it may make orchd run a program.
func (g Git) run(ctx context.Context, checkout string, args ...string) (int, string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := append([]string{"-C", checkout, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String(), nil
	}
	if err != nil {
		return -1, "", err
	}
	return 0, out.String(), nil
}

// Tracked reports whether checkout's settings file is tracked by git.
func (g Git) Tracked(ctx context.Context, checkout string) (bool, error) {
	return g.tracked(ctx, checkout, LocalPath)
}

// tracked reports whether git tracks path, or anything under it, in
// checkout.
func (g Git) tracked(ctx context.Context, checkout, path string) (bool, error) {
	code, _, err := g.run(ctx, checkout, "ls-files", "--error-unmatch", "--", path)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, fmt.Errorf("git ls-files in %s exited %d", checkout, code)
}

// TrackedEntry reports whether git tracks .claude itself (a committed link)
// or the settings file: orch never moves or writes a tracked entry.
func (g Git) TrackedEntry(ctx context.Context, checkout string) (bool, error) {
	if t, err := g.tracked(ctx, checkout, LocalPath); err != nil || t {
		return t, err
	}
	// A committed link at .claude is the entry itself; a tracked file
	// inside a real .claude folder is not.
	code, out, err := g.run(ctx, checkout, "ls-files", "-s", "--", claudeDir)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("git ls-files in %s exited %d", checkout, code)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, "\t"+claudeDir) {
			return true, nil
		}
	}
	return false, nil
}

// Ignored reports whether git ignores checkout's settings file.
func (g Git) Ignored(ctx context.Context, checkout string) (bool, error) {
	code, _, err := g.run(ctx, checkout, "check-ignore", "-q", "--", LocalPath)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore in %s exited %d", checkout, code)
}

// EnsureIgnored makes git ignore checkout's settings file. It adds a line
// to the checkout's info/exclude only when no rule ignores the file
// already, and never edits a .gitignore, which is tracked. It reports
// whether it added the line, and refuses a checkout whose file is tracked.
func (g Git) EnsureIgnored(ctx context.Context, checkout string) (bool, error) {
	if tracked, err := g.Tracked(ctx, checkout); err != nil || tracked {
		if tracked {
			return false, ErrTracked
		}
		return false, err
	}
	if ok, err := g.Ignored(ctx, checkout); err != nil || ok {
		return false, err
	}
	code, out, err := g.run(ctx, checkout, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("git rev-parse in %s exited %d", checkout, code)
	}
	path := strings.TrimSpace(out)
	if !filepath.IsAbs(path) {
		path = filepath.Join(checkout, path)
	}
	if err := appendLine(path, excludeLine); err != nil {
		return false, err
	}
	if ok, err := g.Ignored(ctx, checkout); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%s still not ignored after adding it to %s", LocalPath, path)
		}
		return true, err
	}
	return true, nil
}

// appendLine adds line to path, which must be <git dir>/info/exclude: a
// folder holding HEAD, then info and exclude, neither of them a link, are
// opened one by one. Reason: the session in a checkout can point its .git
// file, or a link at info, anywhere, so orchd writes only where git keeps
// an exclude file, and creates nothing but the info folder.
func appendLine(path, line string) error {
	if filepath.Base(path) != "exclude" || filepath.Base(filepath.Dir(path)) != "info" {
		return fmt.Errorf("%s is not a git info/exclude file", path)
	}
	gitDir := filepath.Dir(filepath.Dir(path))
	if info, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a git folder", gitDir)
	}
	top, err := openDir(-1, gitDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", gitDir, err)
	}
	defer unix.Close(top)
	dir, err := openDir(top, "info")
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(top, "info", 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		dir, err = openDir(top, "info")
	}
	if err != nil {
		return fmt.Errorf("open %s/info: %w", gitDir, err)
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, "exclude", unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	prefix := ""
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n"
		}
	}
	_, werr := f.WriteString(prefix + line + "\n")
	return errors.Join(werr, f.Close())
}
