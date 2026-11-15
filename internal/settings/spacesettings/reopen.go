package spacesettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/willesq/orchestrator/internal/spaces"
)

// The tool guard's check of a checkout's own settings (D-67, RC-215). An
// allowRead or allowWrite that orch did not write re-opens orch's sandbox
// denies for sessions started in the checkout (Loosening), and orch may not
// remove it: a tracked .claude/settings.json is the repository's, and the
// user's entries in settings.local.json are kept (SP-046.1.a2). So the
// guard refuses tool calls in such a checkout until the entry goes.

// maxStateFile bounds how much of orchd's record the guard reads.
const maxStateFile = 32 << 20

// maxReopenDepth bounds the folders checked between a directory and the
// checkout holding it.
const maxReopenDepth = 64

// Reopened lists, for the checkouts orchd's record (stateFile) holds that
// contain one of dirs and whose file holds orch's sandbox denies, every
// entry of their own settings that re-opens a denied folder, and every
// settings file there orch cannot check. Each .claude folder from a
// directory up to its checkout is read, since a session may start in a
// subfolder. A missing record is nothing to check; a record that cannot be
// read is an error, so the guard fails closed (INV-13).
func Reopened(stateFile string, dirs []string) ([]string, error) {
	st, err := readState(stateFile)
	if err != nil || len(st.Checkouts) == 0 {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			continue
		}
		for _, f := range withReal([]string{d}) {
			for c, fs := range st.Checkouts {
				if len(fs.Deny) == 0 {
					continue
				}
				// Reason: the record names a checkout as found under its
				// root, a session's folder may be its real path.
				cs := []string{filepath.Clean(c)}
				if fs.Real != "" {
					cs = append(cs, filepath.Clean(fs.Real))
				}
				holds := func(p string) bool {
					return slices.ContainsFunc(cs, func(c string) bool { return spaces.RootHolds(c, p) })
				}
				if !holds(f) {
					continue
				}
				check := func(dir string, owned Owned) {
					if !seen[dir] {
						seen[dir] = true
						for _, l := range Loosening(dir, fs.Deny, owned) {
							out = append(out, dir+": "+l)
						}
					}
				}
				// Reason: the checkout's own folder first, so a session
				// working deeper than maxReopenDepth below it cannot skip
				// the files it started with.
				check(filepath.Clean(c), fs.Owned)
				for i, dir := 0, f; i < maxReopenDepth && holds(dir) && !slices.Contains(cs, dir); i, dir = i+1, filepath.Dir(dir) {
					// Reason: a subfolder that is gone, or is a link (its
					// target is walked as the folder's real form), has no
					// settings of its own to load; refusing for it would
					// refuse every call there.
					if info, err := os.Lstat(dir); err != nil || info.Mode()&os.ModeSymlink != 0 {
						continue
					}
					check(dir, Owned{})
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// readState reads orchd's record without following a link, at most
// maxStateFile bytes.
func readState(path string) (state, error) {
	st := state{}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read orchd's record of the checkout settings: %w", err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return st, fmt.Errorf("orchd's record of the checkout settings is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateFile+1))
	if err != nil {
		return st, fmt.Errorf("read orchd's record of the checkout settings: %w", err)
	}
	if len(b) > maxStateFile {
		return st, fmt.Errorf("orchd's record of the checkout settings is larger than %d bytes", maxStateFile)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("orchd's record of the checkout settings is not valid: %w", err)
	}
	return st, nil
}
