package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// purgeKeep are the data directory entries --purge-data keeps: the backups
// of every file install and uninstall changed, the only way back.
var purgeKeep = []string{"backups"}

// configLeftovers are files in orch's config directory that only older
// releases and `orch rag eval` write; install's own files there are edits.
var configLeftovers = []string{"config.yaml", "rag-eval.yaml"}

// PurgeTargets lists every path --purge-data deletes: each entry of the data
// directory but backups/, orchd's log directory under ~/Library/Logs, and
// the config directory's leftovers. It refuses a data directory that is not
// plainly orch's own (see checkDataDir).
func (m Machine) PurgeTargets() ([]string, error) {
	if err := checkDataDir(m.DataDir, m.Home); err != nil {
		return nil, err
	}
	var out []string
	entries, err := os.ReadDir(m.DataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !slices.Contains(purgeKeep, e.Name()) {
			out = append(out, filepath.Join(m.DataDir, e.Name()))
		}
	}
	if logs := orchdLogDir(m.Home); exists(logs) {
		out = append(out, logs)
	}
	if m.ConfigDir != "" {
		for _, n := range configLeftovers {
			if p := filepath.Join(m.ConfigDir, n); exists(p) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// orchdLogDir is where the launchd agent install writes sends orchd's logs.
func orchdLogDir(home string) string { return filepath.Join(home, "Library", "Logs", "orchestrator") }

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// checkDataDir refuses to purge a data directory that is not plainly orch's:
// relative, the root, the home or a folder holding it, a symlink, or one
// neither named orch nor holding orch.db. Reason: ORCH_DATA_DIR and
// XDG_DATA_HOME can point anywhere, and a purge of the wrong folder would
// delete the user's files.
func checkDataDir(dir, home string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("data directory %q is not absolute — refusing to purge it", dir)
	}
	dir = filepath.Clean(dir)
	home = filepath.Clean(home)
	if dir == string(filepath.Separator) || dir == home || within(home, dir) {
		return fmt.Errorf("data directory %s is the root, your home or holds it — refusing to purge it", dir)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("data directory %s is a symlink or not a directory — refusing to purge it", dir)
	}
	if dir != defaultDataDir(home) && !exists(filepath.Join(dir, "orch.db")) {
		return fmt.Errorf("data directory %s holds no orch.db and is not the default %s — refusing to purge it",
			dir, defaultDataDir(home))
	}
	return nil
}

// defaultDataDir is the data directory when no variable moves it.
func defaultDataDir(home string) string {
	return filepath.Join(filepath.Clean(home), ".local", "share", "orch")
}

// purgeSteps deletes each purge target, one step each, so the dry run lists
// every path. Reason: when the caller confirmed a list (PurgeList), exactly
// that list is deleted, never a path that appeared after the confirmation.
func (m Machine) purgeSteps() []Step {
	targets, err := m.PurgeList, error(nil)
	if targets == nil {
		targets, err = m.PurgeTargets()
	}
	if err != nil {
		return []Step{{Summary: "data: purge " + m.DataDir, Run: func(context.Context) (string, error) { return "", err }}}
	}
	steps := make([]Step, 0, len(targets))
	for _, t := range targets {
		steps = append(steps, Step{
			Summary: "data: delete " + t,
			Run:     func(context.Context) (string, error) { return "deleted", removeBeneath(t) },
		})
	}
	return steps
}

// removeBeneath deletes path and everything in it through an os.Root opened
// on its parent. Reason: the root confines every step of the removal to the
// parent directory, so a symlink planted inside (or swapped in for the
// path itself) is removed as a link and never followed out of it.
func removeBeneath(path string) error {
	parent, name := filepath.Split(filepath.Clean(path))
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("refusing to delete %q", path)
	}
	root, err := os.OpenRoot(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(name)
}
