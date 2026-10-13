package install

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// assets holds the subagents and skills shipped with the CLI, so
// an install never depends on a separate checkout being present on disk.
//
//go:embed assets
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

// claudeAssetEdits installs the orchestrator's Claude Code subagents into
// ~/.claude/agents and its skills into ~/.claude/skills.
func claudeAssetEdits(home string) []Edit {
	var edits []Edit
	agents, _ := fs.Glob(assets, "assets/claude/agents/*.md")
	for _, src := range agents {
		dst := filepath.Join(home, ".claude", "agents", path.Base(src))
		label := "agent " + strings.TrimSuffix(path.Base(src), ".md")
		edits = append(edits, assetEdit(home, src, dst, []string{dst}, label))
	}
	skills, _ := fs.Glob(assets, "assets/claude/skills/*/SKILL.md")
	for _, src := range skills {
		name := path.Base(path.Dir(src))
		dir := filepath.Join(home, ".claude", "skills", name)
		dst := filepath.Join(dir, "SKILL.md")
		// The legacy install linked the skill directory rather than the file.
		edits = append(edits, assetEdit(home, src, dst, []string{dir, dst}, "skill "+name))
	}
	return edits
}

// assetEdit installs one embedded asset at dst. It creates a missing file,
// refreshes one carrying managedMarker, and replaces a symlink at any of links
// that points into the legacy checkout. Anything else is the user's and fails the
// whole plan rather than being overwritten.
func assetEdit(home, src, dst string, links []string, label string) Edit {
	return Edit{
		Path:   dst,
		Unlink: links,
		Compute: func(old []byte) ([]byte, string, error) {
			body, err := assets.ReadFile(src)
			if err != nil {
				return nil, "", err
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
			default:
				return nil, "", fmt.Errorf("refusing to overwrite %s: it lacks the %q marker, so it is not orch-managed; move it aside and re-run",
					dst, managedMarker)
			}
		},
	}
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
