package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// The git-hook registry records every hooks directory `orch guard git-hook
// --install --apply` wrote into, so `orch install` can keep those hooks
// current and `orch uninstall` can find and remove them. Hooks installed
// before the registry existed are not in it; uninstall says so.

// gitHookRegistry is the file's shape.
type gitHookRegistry struct {
	HooksDirs []string `json:"hooks_dirs"`
}

// GitHookRegistryPath is the registry file under orch's data directory.
func GitHookRegistryPath(dataDir string) string { return filepath.Join(dataDir, "git-hooks.json") }

// RegisteredGitHookDirs lists the recorded hooks directories; a missing
// registry lists none.
func RegisteredGitHookDirs(dataDir string) ([]string, error) {
	b, err := os.ReadFile(GitHookRegistryPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var reg gitHookRegistry
	if err := json.Unmarshal(b, &reg); err != nil {
		return nil, err
	}
	// Reason: only absolute paths are kept, so an entry can never resolve
	// against whatever directory orch happens to run in.
	return slices.DeleteFunc(reg.HooksDirs, func(d string) bool { return !filepath.IsAbs(d) }), nil
}

// RecordGitHookDir adds (installed) or drops a hooks directory in the
// registry, writing owner-only beside it and renaming over.
func RecordGitHookDir(dataDir, hooksDir string, installed bool) error {
	dirs, err := RegisteredGitHookDirs(dataDir)
	if err != nil {
		return err
	}
	hooksDir = filepath.Clean(hooksDir)
	had := slices.Contains(dirs, hooksDir)
	switch {
	case installed && !had:
		dirs = append(dirs, hooksDir)
		slices.Sort(dirs)
	case !installed && had:
		dirs = slices.DeleteFunc(dirs, func(d string) bool { return d == hooksDir })
	default:
		return nil
	}
	b, err := json.MarshalIndent(gitHookRegistry{HooksDirs: dirs}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(GitHookRegistryPath(dataDir), append(b, '\n'))
}

// writePrivate replaces path with content, owner-only, through a temporary
// file renamed over it, so a reader never sees half a file.
func writePrivate(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(content)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
