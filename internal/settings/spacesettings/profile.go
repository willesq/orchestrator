package spacesettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/settings"
)

// profileLinks are what a space's Claude Code profile shares with the
// user's own configuration folder, so a launched session has the user's
// instructions, agents, commands, skills and plugins. Settings are copied
// instead (with the space's rules merged in), and the login is the
// profile's own (SP-004: a fresh CLAUDE_CONFIG_DIR starts logged out).
var profileLinks = []string{"CLAUDE.md", "agents", "commands", "skills", "plugins"}

// WriteClaudeProfile writes the space's Claude Code profile for 'orch run'
// and returns its folder (the session's CLAUDE_CONFIG_DIR). Its
// settings.json is the user's settings.json in userDir with frag merged in,
// rewritten at every launch so it follows the user's own changes (orch's
// hooks included). Each shared entry is linked to the user's unless the
// profile has its own file there.
func WriteClaudeProfile(l Layout, space, userDir string, frag Fragment) (string, error) {
	dir := l.ClaudeDir(space)
	if err := EnsurePrivateDir(l.SpaceDir(space)); err != nil {
		return "", err
	}
	if err := EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	user, err := settings.ReadFile(filepath.Join(userDir, "settings.json"))
	if err != nil {
		return "", err
	}
	m, err := Merge(user, user != nil, frag, Owned{})
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Join(userDir, "settings.json"), err)
	}
	data := m.Data
	if data == nil {
		data = []byte("{}\n")
	}
	if err := replaceFile(filepath.Join(dir, "settings.json"), data); err != nil {
		return "", err
	}
	for _, name := range profileLinks {
		if err := linkShared(filepath.Join(userDir, name), filepath.Join(dir, name)); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// replaceFile writes path owner-only through a temp file and rename, so a
// link at path is replaced rather than followed.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".orch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// linkShared links at to target when the user has target, replacing only a
// link orch made before; a real file or folder at stays the profile's own.
func linkShared(target, at string) error {
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return removeLink(at)
		}
		return err
	}
	info, err := os.Lstat(at)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink == 0:
		return nil
	default:
		if cur, err := os.Readlink(at); err == nil && cur == target {
			return nil
		}
		if err := os.Remove(at); err != nil {
			return err
		}
	}
	return os.Symlink(target, at)
}

// removeLink removes at only when it is a link.
func removeLink(at string) error {
	info, err := os.Lstat(at)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return os.Remove(at)
}

// CodexProfile is the name of the space's Codex permissions profile.
func CodexProfile(space string) string { return "orch_" + strings.ReplaceAll(space, "-", "_") }

// CodexArgs are the config overrides that give a Codex session the rules:
// a permissions profile built on ':workspace' (which lets the shell write
// the workspace and $TMPDIR) that denies every denied folder. SP-004
// measured that Codex has no per-checkout path for this; a profile given
// with -c holds, and a checkout's own config cannot weaken it.
func CodexArgs(r Rules) []string {
	name := CodexProfile(r.Space)
	var fs []string
	for _, p := range r.Deny {
		fs = append(fs, tomlString(p)+"="+tomlString("deny"))
	}
	table := fmt.Sprintf("{extends=%s,filesystem={%s}}", tomlString(":workspace"), strings.Join(fs, ","))
	return []string{
		"-c", "default_permissions=" + tomlString(name),
		"-c", "permissions." + name + "=" + table,
	}
}

// tomlString quotes s as a TOML basic string. JSON's escapes are a subset
// of TOML's.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
