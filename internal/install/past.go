package install

import (
	"os"
	"path/filepath"
)

// What earlier orch versions installed and this one no longer does. Each is
// found by its exact identity (orch's marker, its block markers, its file
// name) and undone like any other edit: backed up first, a symlink never
// deleted. docs/internals/install.md lists every past artifact and how it
// is recognised, including those that need nothing here because a current
// unit already reverses them.

// GittreePath is where orch install once put the gittree worktree helper
// (with an `alias gittree` in the managed .zprofile block). gittree now
// ships separately; a copy there carrying orch's managed-by marker is orch's.
func GittreePath(home string) string {
	return filepath.Join(home, ".config", "orch", "bin", "gittree")
}

// pastToolsUnit removes the gittree helper earlier releases installed. The
// alias that pointed at it lived in the managed .zprofile block, which the
// shell alias unit removes whole.
func pastToolsUnit(home string) Unit {
	path := GittreePath(home)
	return Unit{
		Name:        "gittree helper (earlier releases)",
		Shared:      true,
		InstalledBy: "orch install in releases that shipped the gittree helper (it now ships separately)",
		Past: []Edit{{Path: path, Reverse: func(cur []byte) (Undo, error) {
			return undoOwnedFile(cur, "gittree helper an earlier orch installed", IsManaged)
		}}},
		Dirs: []string{filepath.Dir(path)},
	}
}

// codexOtherEntry is the Codex entry file install does not write now: the
// other of AGENTS.md and AGENTS.override.md (codexEntry). An earlier install
// wrote orch's block into whichever Codex loaded then.
func codexOtherEntry(home string) string {
	if cur := codexEntry(home); filepath.Base(cur) == "AGENTS.md" {
		return filepath.Join(home, ".codex", "AGENTS.override.md")
	}
	return filepath.Join(home, ".codex", "AGENTS.md")
}

// codexPastEntryUnit takes orch's block out of the Codex entry file an
// earlier install wrote, when AGENTS.override.md has since appeared or gone.
func codexPastEntryUnit(home string) Unit {
	return Unit{
		Name:        "codex instructions (earlier entry file)",
		InstalledBy: "orch install, when Codex loaded the other of AGENTS.md and AGENTS.override.md",
		Past:        []Edit{{Path: codexOtherEntry(home), Reverse: cutManagedInstructions}},
	}
}

// homebrewServicePlist is the launchd agent `brew services start orch`
// writes. It is Homebrew's file, so uninstall names it rather than removing it.
func homebrewServicePlist(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", "homebrew.mxcl.orch.plist")
}

// strayHomebrewService names Homebrew's service file for orch when orchd is
// no longer Homebrew-managed (orch was moved to a source build), since
// nothing else in the uninstall stops it.
func strayHomebrewService(home string, homebrew bool) []string {
	if homebrew {
		return nil
	}
	p := homebrewServicePlist(home)
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return []string{"Homebrew's service file for orch is still at " + p +
		": `brew services stop orch` removes it (it is Homebrew's, so orch leaves it)"}
}
