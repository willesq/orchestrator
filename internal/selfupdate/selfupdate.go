// Package selfupdate lets a running orchd notice that a different build of
// itself has been installed and step aside, so the new binary takes over
// without anyone having to run `orch restart`.
//
// It deliberately downloads nothing. orchd is always launched by a KeepAlive
// supervisor — Homebrew's `brew services` plist, or the launchd agent `orch
// install` writes — and both name a *stable* path (/opt/homebrew/opt/orch/bin/
// orchd, or the built binary) rather than a versioned one. `brew upgrade orch`
// relinks that path and leaves the running process serving the old code.
// Exiting is therefore the whole upgrade: the supervisor relaunches from the
// same path seconds later, and that path now resolves to the new build.
package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Build identifies the binary sitting behind a launch path. The resolved path
// is what catches a Homebrew upgrade (…/Cellar/orch/0.4.1/… becomes …/0.4.3/…);
// size and mtime catch a source build that overwrites the binary in place,
// where the path never changes.
type Build struct {
	Path    string // symlinks resolved
	Size    int64
	ModTime time.Time
}

// String renders a build for a log line. The resolved path carries the version
// for a Homebrew install, and the mtime separates two builds at the same path.
func (b Build) String() string {
	return fmt.Sprintf("%s, %d bytes, built %s", b.Path, b.Size, b.ModTime.Format(time.RFC3339))
}

// same reports whether two builds are the same artifact. ModTime is compared
// with Equal rather than ==, which also weighs the monotonic clock and location.
func (b Build) same(o Build) bool {
	return b.Path == o.Path && b.Size == o.Size && b.ModTime.Equal(o.ModTime)
}

// Inspect identifies the build currently behind launchPath.
func Inspect(launchPath string) (Build, error) {
	resolved, err := filepath.EvalSymlinks(launchPath)
	if err != nil {
		return Build{}, err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return Build{}, err
	}
	if !fi.Mode().IsRegular() {
		return Build{}, fmt.Errorf("selfupdate: %s is not a regular file", resolved)
	}
	if fi.Size() == 0 {
		return Build{}, fmt.Errorf("selfupdate: %s is empty", resolved)
	}
	return Build{Path: resolved, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Watcher compares the build behind a launch path against the one that was
// there when the process started.
type Watcher struct {
	path     string
	baseline Build
}

// NewWatcher records the build orchd is running right now, so every later
// comparison is against the code actually in memory.
func NewWatcher(launchPath string) (*Watcher, error) {
	b, err := Inspect(launchPath)
	if err != nil {
		return nil, err
	}
	return &Watcher{path: launchPath, baseline: b}, nil
}

// Path is the launch path being watched.
func (w *Watcher) Path() string { return w.path }

// Baseline is the build the process is running.
func (w *Watcher) Baseline() Build { return w.baseline }

// Changed reports whether a different build now sits behind the launch path.
//
// An unreadable path is deliberately not a change: an upgrade unlinks before it
// relinks, so a missing or half-linked binary is a moment to wait through, not
// a reason to hand the supervisor something that may not even exec.
func (w *Watcher) Changed() (Build, bool) {
	b, err := Inspect(w.path)
	if err != nil {
		return Build{}, false
	}
	return b, !b.same(w.baseline)
}

// Adopt treats b as the running build, so a change that was seen but not acted
// on is not re-reported on every check.
func (w *Watcher) Adopt(b Build) { w.baseline = b }

// Supervised reports whether something will start orchd again if it exits.
//
// Both supported install routes are launchd jobs with KeepAlive, and launchd
// runs its jobs as children of pid 1. A foreground `orchd` run has a real shell
// as its parent, and exiting there would turn a stale daemon into no daemon at
// all — strictly worse than the version drift it is trying to fix.
func Supervised() bool { return os.Getppid() == 1 }
