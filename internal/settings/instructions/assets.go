package instructions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/settings"
)

// The Claude Code subagents and skills orch ships are items too: on, every
// file is installed; off, every file orch installed is removed. Files that
// are the user's own (no orch marker, and not identical to the shipped copy)
// are never overwritten or removed.

// readAssets adds every file of every shipped asset to files. A file or an
// asset directory that is a symlink or otherwise not regular is recorded in
// notRegular and not read, so Enforce neither writes nor deletes through it.
func (p *Provider) readAssets(files map[string][]byte) error {
	list, err := p.assets()
	if err != nil {
		return err
	}
	for _, a := range list {
		linkedDir := false
		for _, d := range a.Dirs {
			if info, err := os.Lstat(install.ClaudeAssetPath(p.home, d)); err == nil && !info.IsDir() {
				linkedDir = true
			}
		}
		for _, f := range a.Files {
			path := install.ClaudeAssetPath(p.home, f.Rel)
			if info, err := os.Lstat(path); linkedDir || (err == nil && !info.Mode().IsRegular()) {
				p.notRegular[path] = true
				files[path] = nil
				continue
			}
			b, err := settings.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = b
		}
	}
	return nil
}

// observeAssets lists every shipped asset, on by default.
func (p *Provider) observeAssets() ([]settings.Item, error) {
	list, err := p.assets()
	if err != nil {
		return nil, err
	}
	out := make([]settings.Item, 0, len(list))
	for _, a := range list {
		paths := make([]string, 0, len(a.Files))
		for _, f := range a.Files {
			paths = append(paths, install.ClaudeAssetPath(p.home, f.Rel))
		}
		group := "Subagents"
		if a.Kind == install.KindSkill {
			group = "Skills"
		}
		payload, err := json.Marshal(Payload{Title: a.Title, Group: group, Description: a.Description,
			Body: install.DisplayBody(a.Main().Body), Path: paths[0], Files: paths, Default: true, Sensitive: a.Sensitive})
		if err != nil {
			return nil, err
		}
		out = append(out, settings.Item{Kind: a.Kind, Identity: a.Name, Payload: payload, Enabled: true,
			Origin: settings.OriginOrch})
	}
	return out, nil
}

// enforceAssets adds to c what puts each asset's files in its stored state,
// and returns the files it had to leave alone.
func (p *Provider) enforceAssets(files map[string][]byte, enabled func(kind, identity string) (bool, bool), c *settings.Changes) ([]error, error) {
	list, err := p.assets()
	if err != nil {
		return nil, err
	}
	var problems []error
	for _, a := range list {
		on := true
		if v, known := enabled(a.Kind, a.Name); known {
			on = v
		}
		linked := false
		for _, f := range a.Files {
			path := install.ClaudeAssetPath(p.home, f.Rel)
			if p.notRegular[path] {
				linked = true
				problems = append(problems, fmt.Errorf("%s (%s %s) is or lies under a symlink or non-regular file; orch will not write or remove through it", path, a.Kind, a.Name))
				continue
			}
			old := files[path]
			switch {
			case on && string(old) == string(f.Body):
			case on && (old == nil || f.Pinned || install.IsManaged(old) || install.IsAdoptable(old, f.Body)):
				c.Write[path] = f.Body
			case on:
				problems = append(problems, fmt.Errorf("%s is your own file (no orch marker, differs from the shipped %s %s); orch leaves it — move it aside to let orch install its copy", path, a.Kind, a.Name))
			case old != nil && (f.Pinned || install.IsManaged(old)):
				c.Delete = append(c.Delete, path)
			}
		}
		// Reason: a directory with a symlink anywhere in it is not pruned at
		// all; removing a linked directory would delete the user's link.
		if !on && !linked {
			for _, d := range a.Dirs {
				c.Prune = append(c.Prune, filepath.Clean(install.ClaudeAssetPath(p.home, d)))
			}
		}
	}
	return problems, nil
}
