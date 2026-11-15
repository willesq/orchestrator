package install

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// assets holds the subagents and skills shipped with the CLI, so
// an install never depends on a separate checkout being present on disk.
//
// Reason: all: keeps directories such as agents/_shared, which a plain
// directory pattern silently leaves out for starting with "_".
//
//go:embed all:assets
var assets embed.FS

// managedMarker identifies a file orch install owns and may overwrite. Every
// shipped asset carries it; a file without it belongs to the user.
const managedMarker = "managed-by: orch"

// legacyCheckout is where the retired Python orchestrator kept these assets. Its
// installs symlinked them into the harness, so links pointing inside it are the
// only foreign entries an install replaces on its own.
func legacyCheckout(home string) string {
	return filepath.Join(home, ".claude", "orchestrator")
}

// claudeAssetsUnit installs the orchestrator's Claude Code subagents into
// ~/.claude/agents and its skills into ~/.claude/skills: every file of each
// asset turned on in orch. An asset turned off is left as it is; orchd's
// settings watcher removes its files. Uninstall removes every file carrying
// orch's marker, on or off, and the directories only an asset used.
func claudeAssetsUnit(home string) Unit {
	u := Unit{Name: "claude-code agents and skills", Edits: claudeAssetEdits(home)}
	if list, err := ClaudeAssets(); err == nil {
		for _, a := range list {
			for _, d := range a.Dirs {
				u.Dirs = append(u.Dirs, ClaudeAssetPath(home, d))
			}
		}
	}
	return u
}

func claudeAssetEdits(home string) []Edit {
	list, err := ClaudeAssets()
	if err != nil {
		return []Edit{{Path: filepath.Join(home, ".claude"), Compute: func([]byte) ([]byte, string, error) { return nil, "", err },
			Reverse: func([]byte) (Undo, error) { return Undo{}, err }}}
	}
	var edits []Edit
	for _, a := range list {
		for _, f := range a.Files {
			dst := ClaudeAssetPath(home, f.Rel)
			links := []string{dst}
			if a.Kind == KindSkill {
				// The legacy install linked the skill directory rather than the file.
				links = []string{filepath.Join(home, ".claude", "skills", a.Name), dst}
			}
			label := a.Kind + " " + a.Name
			if f.Rel != a.Main().Rel {
				label += " (" + path.Base(f.Rel) + ")"
			}
			edits = append(edits, assetEdit(home, a, f.Body, dst, links, label))
		}
	}
	return edits
}

// assetEdit installs one asset file at dst. It creates a missing file,
// refreshes one carrying managedMarker, adopts the user's own copy when it is
// exactly the shipped file without the marker, and replaces a symlink at any
// of links that points into the legacy checkout. Anything else is the user's
// and fails the whole plan rather than being overwritten. An asset turned off
// in orch is left as it is.
func assetEdit(home string, a ClaudeAsset, body []byte, dst string, links []string, label string) Edit {
	return Edit{
		Path:   dst,
		Unlink: links,
		Compute: func(old []byte) ([]byte, string, error) {
			// Reason: an unreadable store stops the plan rather than being
			// taken as "nothing is off", which would put every asset back.
			toggles, err := loadToggles()
			if err != nil {
				return nil, "", fmt.Errorf("%w — refusing to write %s, so agents and skills turned off in orch stay off", err, dst)
			}
			if !AssetEnabled(toggles)(a) {
				return old, label + ": turned off in orch; left as it is", nil
			}
			legacy := false
			for _, l := range links {
				target, isLink, err := linkTarget(l)
				if err != nil {
					return nil, "", err
				}
				if !isLink {
					continue
				}
				if !within(target, legacyCheckout(home)) {
					return nil, "", fmt.Errorf("refusing to replace symlink %s -> %s: it does not point into %s, so it is not orch-managed; move it aside and re-run",
						l, target, legacyCheckout(home))
				}
				legacy = true
			}
			switch {
			case legacy:
				return body, label + ": replace link into the retired " + legacyCheckout(home) + " with the copy shipped with this CLI", nil
			case len(old) == 0:
				return body, label + ": install the copy shipped with this CLI", nil
			case bytes.Contains(old, []byte(managedMarker)):
				return body, label + ": update to the copy shipped with this CLI", nil
			case IsAdoptable(old, body):
				return body, label + ": take over your identical copy (adds the orch marker)", nil
			default:
				return nil, "", fmt.Errorf("refusing to overwrite %s: it lacks the %q marker and differs from the shipped copy, so it is yours; move it aside or turn %s off in orch, and re-run",
					dst, managedMarker, label)
			}
		},
		// Reason: only a file carrying orch's marker goes. One without it is
		// the user's (install refused to overwrite it, or they took it over).
		// A link into the retired checkout is left whatever it holds: that
		// checkout is the user's folder, and nothing behind it is emptied.
		Reverse: func(cur []byte) (Undo, error) {
			if intoLegacyCheckout(home, dst, links) {
				return keepUndo(cur, label+": left in place — "+dst+" leads into "+legacyCheckout(home)+
					", which is yours; remove the link by hand"), nil
			}
			return undoOwnedFile(cur, label, IsManaged)
		},
	}
}

// intoLegacyCheckout reports whether dst is, or may be, in the retired
// checkout: one of links points there, or dst resolves there through a link
// anywhere on its path (~/.claude/agents, ~/.claude, or the checkout itself
// being a link). A path that cannot be resolved for another reason than
// being absent counts as there, so nothing behind it is changed.
func intoLegacyCheckout(home, dst string, links []string) bool {
	legacy := legacyCheckout(home)
	for _, l := range links {
		if target, isLink, err := linkTarget(l); err == nil && isLink && within(target, legacy) {
			return true
		}
	}
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return !os.IsNotExist(err)
	}
	if within(real, legacy) {
		return true
	}
	realLegacy, err := filepath.EvalSymlinks(legacy)
	return err == nil && within(real, realLegacy)
}

// linkTarget reports whether p is a symlink and, if so, its absolute target.
func linkTarget(p string) (target string, isLink bool, err error) {
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", false, nil
	}
	target, err = os.Readlink(p)
	if err != nil {
		return "", false, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	return filepath.Clean(target), true, nil
}

// within reports whether p is dir or lies beneath it.
func within(p, dir string) bool {
	dir = filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
