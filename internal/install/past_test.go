package install

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// oldGittree is the gittree helper as earlier releases installed it.
const oldGittree = "#!/usr/bin/env bash\n# managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo.\n" +
	"# Local edits are overwritten on the next install; change the repo copy instead.\necho worktree\n"

// editJSON rewrites a JSON file in orch's encoding.
func editJSON(t *testing.T, path string, change func(root map[string]any)) {
	t.Helper()
	root := map[string]any{}
	if err := json.Unmarshal([]byte(readFile(t, path)), &root); err != nil {
		t.Fatal(err)
	}
	change(root)
	writeFile(t, path, mustJSON(t, root))
}

// TestUninstallRemovesWhatEarlierReleasesInstalled lays what each earlier
// release put in place over a current install, and checks uninstall takes
// all of it out and leaves the user's own files exactly as they were.
func TestUninstallRemovesWhatEarlierReleasesInstalled(t *testing.T) {
	h := newTestHome(t)
	before := h.tree(t)
	units := h.units(AllAdapters()...)
	applyPlan(t, InstallPlan("all", units), h)

	const oldOrch, oldHook = "/old/libexec/orch", "/old/libexec/orch-hook"
	// The gittree helper and its alias, from the releases that shipped it.
	writeFile(t, GittreePath(h.home), oldGittree)
	zp := filepath.Join(h.home, ".zprofile")
	writeFile(t, zp, strings.Replace(readFile(t, zp), aliasEnd,
		"alias gittree=\""+GittreePath(h.home)+"\"\n"+aliasEnd, 1))
	// The first Go releases: rag and queue in settings.json and
	// ~/.claude.json from another bin dir, orch-hook from there, and the
	// retired Python checkout's hook scripts.
	editJSON(t, filepath.Join(h.home, ".claude", "settings.json"), func(root map[string]any) {
		servers := root["mcpServers"].(map[string]any)
		for _, s := range []string{"rag", "queue"} {
			servers[s] = map[string]any{"command": oldOrch, "args": []any{"mcp", s}}
		}
		hooks := root["hooks"].(map[string]any)
		hooks["PreCompact"] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": oldHook + " pre-compact"}}}}
		hooks["SubagentStop"] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": "python3 " + filepath.Join(h.home, ".claude", "orchestrator", "hooks", "subagent_stop.py")}}}}
	})
	editJSON(t, filepath.Join(h.home, ".claude.json"), func(root map[string]any) {
		root["mcpServers"].(map[string]any)["queue"] = map[string]any{"command": oldOrch, "args": []any{"mcp", "queue"}}
	})
	cfg := filepath.Join(h.home, ".codex", "config.toml")
	root := map[string]any{}
	if err := toml.Unmarshal([]byte(readFile(t, cfg)), &root); err != nil {
		t.Fatal(err)
	}
	root["mcp_servers"].(map[string]any)["orch_rag"] = map[string]any{"command": oldOrch, "args": []any{"mcp", "rag"}}
	b, err := marshalTOML(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg, string(b))
	// The launchd agent, pointing at an orchd that has since moved.
	writeFile(t, PlistPath(h.home), plistContent("/old/libexec/orchd", h.home))
	// Codex's block went into AGENTS.md; AGENTS.override.md came later, so
	// install now writes there and no current edit reads AGENTS.md.
	override := filepath.Join(h.home, ".codex", "AGENTS.override.md")
	writeFile(t, override, "override rules\n")
	before[strings.TrimPrefix(override, h.home)] = "override rules\n"
	if !HasManagedBlock([]byte(readFile(t, filepath.Join(h.home, ".codex", "AGENTS.md")))) {
		t.Fatal("setup: no block in AGENTS.md")
	}

	units = h.units(AllAdapters()...)
	out := applyPlan(t, UninstallPlan("all", units, true), h)
	diffTrees(t, before, h.tree(t))
	if _, err := os.Lstat(filepath.Dir(GittreePath(h.home))); err == nil {
		t.Error("the gittree helper's folder was left behind")
	}
	if !strings.Contains(out, "gittree helper an earlier orch installed") {
		t.Errorf("uninstall does not name the gittree helper:\n%s", out)
	}
}

func TestPastArtifactsLeaveTheUsersOwn(t *testing.T) {
	h := newTestHome(t)
	// A gittree of the user's own at the same path, and AGENTS.override.md
	// of their own with no block: both stay.
	writeFile(t, GittreePath(h.home), "#!/bin/sh\necho mine\n")
	writeFile(t, filepath.Join(h.home, ".codex", "AGENTS.override.md"), "mine\n")
	before := h.tree(t)
	var out strings.Builder
	p := UninstallPlan("all", h.units(AllAdapters()...), true)
	if err := p.Print(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "left in place — it is not orch's") {
		t.Errorf("the user's gittree is not named as kept:\n%s", out.String())
	}
	applyPlan(t, p, h)
	after := h.tree(t)
	for _, rel := range []string{"/.config/orch/bin/gittree", "/.codex/AGENTS.override.md", "/.codex/AGENTS.md"} {
		if after[rel] != before[rel] {
			t.Errorf("%s changed: %q", rel, after[rel])
		}
	}
	// A single-harness uninstall leaves the shared gittree unit, and takes
	// Codex's earlier entry file only for Codex.
	if names := unitNames(Codex{}.Units(testBins(), h.home)); !strings.Contains(names, "codex instructions (earlier entry file)") {
		t.Errorf("codex units: %s", names)
	}
	if codexOtherEntry(h.home) != filepath.Join(h.home, ".codex", "AGENTS.md") {
		t.Errorf("other entry = %s", codexOtherEntry(h.home))
	}
}

func unitNames(units []Unit) string {
	var names []string
	for _, u := range units {
		names = append(names, u.Name)
	}
	return strings.Join(names, ", ")
}

func TestDedupeDropsPastEditOfACurrentPath(t *testing.T) {
	rev := func([]byte) (Undo, error) { return Undo{}, nil }
	write := func([]byte) ([]byte, string, error) { return nil, "", nil }
	units := dedupe([]Unit{
		{Name: "past", InstalledBy: "x", Past: []Edit{{Path: "/a", Reverse: rev}, {Path: "/b", Reverse: rev}}},
		{Name: "current", Edits: []Edit{{Path: "/a", Compute: write, Reverse: rev}}},
	})
	if len(units[0].Past) != 1 || units[0].Past[0].Path != "/b" || len(units[1].Edits) != 1 {
		t.Errorf("dedupe = %+v", units)
	}
	// Install never runs a past edit.
	if p := InstallPlan("x", units); len(p.Edits) != 1 {
		t.Errorf("install plan has %d edits", len(p.Edits))
	}
}

func TestStrayHomebrewService(t *testing.T) {
	home := t.TempDir()
	if got := strayHomebrewService(home, false); len(got) != 0 {
		t.Errorf("named a service file that is not there: %v", got)
	}
	writeFile(t, homebrewServicePlist(home), "<plist/>")
	if got := strayHomebrewService(home, true); len(got) != 0 {
		t.Errorf("named Homebrew's file while orchd is Homebrew-managed: %v", got)
	}
	got := strayHomebrewService(home, false)
	if len(got) != 1 || !strings.Contains(got[0], "brew services stop orch") {
		t.Errorf("notes = %v", got)
	}
	m := Machine{Home: home, Bins: testBins(), Run: (&fakeRun{}).run}
	if notes := strings.Join(m.daemonUnit().Leftovers(), "\n"); !strings.Contains(notes, "homebrew.mxcl.orch.plist") {
		t.Errorf("daemon leftovers = %s", notes)
	}
}

// TestUninstallLeavesLinksIntoTheRetiredCheckout: the retired orchestrator
// linked agents and skills from ~/.claude/orchestrator, which is the user's
// folder. Uninstall keeps the links and never empties a file behind them,
// even one carrying orch's marker.
func TestUninstallLeavesLinksIntoTheRetiredCheckout(t *testing.T) {
	h := newTestHome(t)
	checkout := legacyCheckout(h.home)
	agent := filepath.Join(checkout, "agents", "dev.md")
	skill := filepath.Join(checkout, "skills", "qa-fixer")
	writeFile(t, agent, "---\nname: dev\n---\n<!-- managed-by: orch -->\n")
	writeFile(t, filepath.Join(skill, "SKILL.md"), "managed-by: orch\n")
	if err := os.MkdirAll(filepath.Join(h.home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(agent, filepath.Join(h.home, ".claude", "agents", "dev.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(skill, filepath.Join(h.home, ".claude", "skills", "qa-fixer")); err != nil {
		t.Fatal(err)
	}
	p := UninstallPlan("all", h.units(ClaudeCode{}), true)
	var out strings.Builder
	if err := p.Print(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "leads into "+checkout) {
		t.Errorf("the links are not named:\n%s", out.String())
	}
	applyPlan(t, p, h)
	if readFile(t, agent) != "---\nname: dev\n---\n<!-- managed-by: orch -->\n" || readFile(t, filepath.Join(skill, "SKILL.md")) != "managed-by: orch\n" {
		t.Error("a file in the retired checkout was changed")
	}
	for _, l := range []string{filepath.Join(h.home, ".claude", "agents", "dev.md"), filepath.Join(h.home, ".claude", "skills", "qa-fixer")} {
		if info, err := os.Lstat(l); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: link not kept", l)
		}
	}
}

func TestIntoLegacyCheckout(t *testing.T) {
	home := t.TempDir()
	checkout := legacyCheckout(home)
	// ~/.claude/agents is itself a link into the checkout: a file under it
	// is in the checkout although no link names it.
	writeFile(t, filepath.Join(checkout, "agents", "dev.md"), "managed-by: orch\n")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "agents"), filepath.Join(home, ".claude", "agents")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(home, ".claude", "agents", "dev.md")
	if !intoLegacyCheckout(home, dst, []string{dst}) {
		t.Error("a file behind a linked parent folder was not found in the checkout")
	}
	// A file of orch's own, and one that is absent, are not.
	own := filepath.Join(home, ".claude", "skills", "x", "SKILL.md")
	writeFile(t, own, "managed-by: orch\n")
	if intoLegacyCheckout(home, own, []string{own}) || intoLegacyCheckout(home, own+"-gone", nil) {
		t.Error("a path outside the checkout was taken for one in it")
	}
	// A link loop cannot be resolved, so it counts as there.
	loop := filepath.Join(home, ".claude", "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if !intoLegacyCheckout(home, filepath.Join(loop, "x"), nil) {
		t.Error("an unresolvable path was not kept")
	}
	// The uninstall keeps the file behind the linked folder.
	p := UninstallPlan("x", []Unit{claudeAssetsUnitFor(t, home)}, true)
	if err := p.Apply(io.Discard, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(checkout, "agents", "dev.md")) != "managed-by: orch\n" {
		t.Error("a file in the retired checkout was removed")
	}
}

// claudeAssetsUnitFor is the Claude Code assets unit for home.
func claudeAssetsUnitFor(t *testing.T, home string) Unit {
	t.Helper()
	for _, u := range (ClaudeCode{}).Units(testBins(), home) {
		for _, e := range u.Edits {
			if e.Path == filepath.Join(home, ".claude", "agents", "dev.md") {
				return u
			}
		}
	}
	t.Fatal("no unit installs the dev agent")
	return Unit{}
}
