package instructions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/settings"
)

func mustModules(t *testing.T) []install.Module {
	t.Helper()
	mods, err := install.Modules()
	if err != nil {
		t.Fatal(err)
	}
	return mods
}

func importLine(home, slug string) string {
	return "@" + strings.ReplaceAll(install.ModulePath(home, slug), " ", `\ `) + "\n"
}

// TestModuleToggleEndToEnd is the contract between the app, the watcher and
// orch install: orch.db decides which modules load, a toggle rewrites only the
// managed block, the watcher undoes hand edits, and install agrees.
func TestModuleToggleEndToEnd(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("ORCH_DATA_DIR", data)
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	home := t.TempDir()
	entry := filepath.Join(home, ".gemini", "GEMINI.md")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("Personal rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bins := install.Bins{Orch: "/opt/orch/orch", OrchHook: "/opt/orch/orch-hook", Orchd: "/opt/orch/orchd"}
	plan := install.BuildPlan(install.Gemini{}, bins, home)
	backups := filepath.Join(data, "backups")
	if err := plan.Apply(io.Discard, backups); err != nil {
		t.Fatal(err)
	}
	mods := mustModules(t)
	target := mods[3].Slug

	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reg, _ := settings.NewRegistry(New(home))
	svc := settings.NewService(settings.NewStore(conn), reg, install.BackupFunc(backups))
	ctx := context.Background()
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	l, err := svc.List(ctx, install.InstructionsProvider, install.KindModule)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) != len(mods) {
		t.Fatalf("%d rows, want one per module (%d)", len(l.Rows), len(mods))
	}
	for _, r := range l.Rows {
		if r.Status() != settings.StatusActive || r.Origin != settings.OriginOrch {
			t.Errorf("%s: status %s origin %s", r.Identity, r.Status(), r.Origin)
		}
	}
	var p Payload
	if err := json.Unmarshal(l.Rows[0].Payload, &p); err != nil || p.Body == "" || p.Title == "" {
		t.Errorf("payload should carry the module text for the UI: %+v %v", p, err)
	}

	if _, err := svc.SetEnabled(ctx, install.InstructionsProvider, install.KindModule, target, false); err != nil {
		t.Fatal(err)
	}
	got := read()
	if strings.Contains(got, importLine(home, target)) || !strings.Contains(got, importLine(home, mods[0].Slug)) {
		t.Fatalf("toggle off did not drop exactly that import:\n%s", got)
	}
	if !strings.HasPrefix(got, "Personal rule\n") {
		t.Error("personal text outside the block was changed")
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Errorf("install disagrees with the toggle: %d pending, %v", n, err)
	}

	// A hand edit inside the block is drift: the watcher puts the stored state back.
	hand := strings.Replace(got, importLine(home, mods[0].Slug), importLine(home, mods[0].Slug)+importLine(home, target), 1)
	if err := os.WriteFile(entry, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(install.ModulePath(home, mods[0].Slug)); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	if read() != got {
		t.Errorf("hand-added import was not reverted:\n%s", read())
	}
	if b, err := os.ReadFile(install.ModulePath(home, mods[0].Slug)); err != nil || string(b) != mods[0].Body {
		t.Errorf("deleted module file was not restored: %v", err)
	}
	r, _ := settings.NewStore(conn).Get(ctx, install.InstructionsProvider, install.KindModule, target)
	if r.Enabled {
		t.Error("a hand edit overrode the stored choice")
	}

	if _, err := svc.SetEnabled(ctx, install.InstructionsProvider, install.KindModule, target, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), importLine(home, mods[2].Slug)+importLine(home, target)+importLine(home, mods[4].Slug)) {
		t.Errorf("re-enabled module not back in order:\n%s", read())
	}
	if err := plan.Apply(io.Discard, backups); err != nil {
		t.Fatal(err)
	}
	if n, _ := plan.Pending(); n != 0 {
		t.Errorf("%d pending after re-install", n)
	}
}

func TestEnforce(t *testing.T) {
	home := t.TempDir()
	p := New(home)
	mods := mustModules(t)
	none := func(string, string) (bool, bool) { return false, false }

	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := p.Enforce(files, none); err != nil || !c.Empty() {
		t.Errorf("an unwired home was changed: %+v, %v", c, err)
	}

	codex := filepath.Join(home, ".codex", "AGENTS.md")
	files[codex] = []byte("Mine\n<!-- BEGIN orchestrator-managed -->\nold\n<!-- END orchestrator-managed -->\n")
	off := mods[0].Slug
	ch, err := p.Enforce(files, func(kind, id string) (bool, bool) {
		return id != off, id == off
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := ch.Write
	if len(changed) != len(mods)+1 {
		t.Errorf("%d files changed, want the entry and every module file", len(changed))
	}
	body := string(changed[codex])
	if !strings.HasPrefix(body, "Mine\n") || strings.Contains(body, off+".md") || !strings.Contains(body, mods[1].Slug+".md") {
		t.Errorf("codex entry = %q", body)
	}

	// Broken markers in one entry are reported without holding up another.
	gemini := filepath.Join(home, ".gemini", "GEMINI.md")
	files[codex] = []byte("<!-- BEGIN orchestrator-managed -->\n<!-- BEGIN orchestrator-managed -->\n")
	files[gemini] = []byte("<!-- BEGIN orchestrator-managed -->\nold\n<!-- END orchestrator-managed -->\n")
	ch, err = p.Enforce(files, none)
	changed = ch.Write
	if err == nil || !strings.Contains(err.Error(), codex) {
		t.Errorf("broken markers not reported: %v", err)
	}
	if _, wrote := changed[codex]; wrote {
		t.Error("broken markers were rewritten")
	}
	if !strings.Contains(string(changed[gemini]), mods[0].Slug+".md") {
		t.Error("a broken entry held up a healthy one")
	}

	p.modules = func() ([]install.Module, error) { return nil, errors.New("bad build") }
	if _, err := p.Read(); err == nil {
		t.Error("Read hid a module parse error")
	}
	if _, err := p.Observe(nil); err == nil {
		t.Error("Observe hid a module parse error")
	}
	if _, err := p.Enforce(nil, none); err == nil {
		t.Error("Enforce hid a module parse error")
	}
}

// TestEnforceRefusesSymlinkedModules: the watcher writes on its own, so a
// module path planted as a symlink must never redirect its write.
func TestEnforceRefusesSymlinkedModules(t *testing.T) {
	home := t.TempDir()
	mods := mustModules(t)
	entry := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("<!-- BEGIN orchestrator-managed -->\n<!-- END orchestrator-managed -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "zshrc")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := install.ModulePath(home, mods[0].Slug)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, link); err != nil {
		t.Fatal(err)
	}
	p := New(home)
	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	ch, problems := p.Enforce(files, func(string, string) (bool, bool) { return false, false })
	changed := ch.Write
	if _, wrote := changed[link]; wrote {
		t.Fatal("Enforce would write through a symlinked module file")
	}
	if problems == nil || !strings.Contains(problems.Error(), link) {
		t.Errorf("symlinked module not reported: %v", problems)
	}
	if _, ok := changed[install.ModulePath(home, mods[1].Slug)]; !ok || changed[entry] == nil {
		t.Error("one bad module file held up the rest")
	}
	if b, _ := os.ReadFile(sentinel); string(b) != "keep me\n" {
		t.Errorf("sentinel changed: %q", b)
	}
}

func TestProviderSurface(t *testing.T) {
	p := New(t.TempDir())
	if p.Name() != install.InstructionsProvider || p.Title() == "" || len(p.Kinds()) != 3 || !p.Kinds()[0].StateInFile {
		t.Errorf("surface = %s %s %+v", p.Name(), p.Title(), p.Kinds())
	}
	if _, err := p.Apply(nil, settings.Item{}, true); !errors.Is(err, settings.ErrNotAllowed) {
		t.Errorf("Apply = %v", err)
	}
	if _, err := p.Validate(install.KindModule, nil); !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("Validate = %v", err)
	}
	group, summary := p.Describe(install.KindModule, json.RawMessage(`{"title":"T","group":"G","description":"D"}`))
	if group != "G" || summary != "T: D" {
		t.Errorf("Describe = %q %q", group, summary)
	}
	if g, s := p.Describe(install.KindModule, json.RawMessage(`nope`)); g != "" || s != "" {
		t.Errorf("Describe(bad) = %q %q", g, s)
	}
	if _, s := p.Describe(install.KindModule, json.RawMessage(`{"title":"T"}`)); s != "T" {
		t.Errorf("Describe without description = %q", s)
	}
}

// assetHome is a home with Claude Code wired by orch, ready for Enforce.
func assetHome(t *testing.T) (string, *Provider) {
	t.Helper()
	home := t.TempDir()
	entry := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("<!-- BEGIN orchestrator-managed -->\n<!-- END orchestrator-managed -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, New(home)
}

func findAsset(t *testing.T, kind, name string) install.ClaudeAsset {
	t.Helper()
	list, err := install.ClaudeAssets()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Kind == kind && a.Name == name {
			return a
		}
	}
	t.Fatalf("%s %s not shipped", kind, name)
	return install.ClaudeAsset{}
}

// TestAssetToggleEndToEnd: turning a skill off removes every file orch
// installed and the directories it emptied; turning it on puts them back; a
// file of the user's own is never touched.
func TestAssetToggleEndToEnd(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	home, p := assetHome(t)
	fixer := findAsset(t, install.KindSkill, "qa-fixer")
	api := findAsset(t, install.KindSkill, "api-design")
	apiPath := install.ClaudeAssetPath(home, api.Main().Rel)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var backups []string
	reg, _ := settings.NewRegistry(p)
	svc := settings.NewService(settings.NewStore(conn), reg, func(path string, _ []byte) error {
		backups = append(backups, path)
		return nil
	})
	ctx := context.Background()
	svc.Sync(ctx)
	for _, f := range fixer.Files {
		if b, err := os.ReadFile(install.ClaudeAssetPath(home, f.Rel)); err != nil || string(b) != string(f.Body) {
			t.Fatalf("%s not installed: %v", f.Rel, err)
		}
	}
	if _, err := os.Stat(apiPath); err != nil {
		t.Fatal(err)
	}

	// A file the user added inside the skill keeps its directory.
	mine := filepath.Join(home, ".claude", "skills", "qa-fixer", "references", "mine.md")
	if err := os.WriteFile(mine, []byte("my notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(ctx, install.InstructionsProvider, install.KindSkill, "qa-fixer", false); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixer.Files {
		if _, err := os.Stat(install.ClaudeAssetPath(home, f.Rel)); !os.IsNotExist(err) {
			t.Errorf("%s still present after turning qa-fixer off", f.Rel)
		}
	}
	if b, err := os.ReadFile(mine); err != nil || string(b) != "my notes\n" {
		t.Error("the user's own file inside the skill was removed")
	}
	if len(backups) == 0 {
		t.Error("removed files were not backed up")
	}
	if err := os.Remove(mine); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "qa-fixer")); !os.IsNotExist(err) {
		t.Errorf("emptied skill directory was not removed: %v", err)
	}

	// A hand-restored copy of an off skill is orch's (marked), so it is removed again.
	main := install.ClaudeAssetPath(home, fixer.Main().Rel)
	if err := os.MkdirAll(filepath.Dir(main), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, fixer.Main().Body, 0o644); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	if _, err := os.Stat(main); !os.IsNotExist(err) {
		t.Error("a marked copy of an off skill was left in place")
	}

	if _, err := svc.SetEnabled(ctx, install.InstructionsProvider, install.KindSkill, "qa-fixer", true); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixer.Files {
		if b, err := os.ReadFile(install.ClaudeAssetPath(home, f.Rel)); err != nil || string(b) != string(f.Body) {
			t.Errorf("%s not restored: %v", f.Rel, err)
		}
	}
}

func TestEnforceAssetsLeavesUserFilesAndLinks(t *testing.T) {
	home, p := assetHome(t)
	api := findAsset(t, install.KindSkill, "api-design")
	fixer := findAsset(t, install.KindSkill, "qa-fixer")
	qa := findAsset(t, install.KindAgent, "adversarial-qa")

	// api-design: the user's own different copy.
	apiPath := install.ClaudeAssetPath(home, api.Main().Rel)
	if err := os.MkdirAll(filepath.Dir(apiPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apiPath, []byte("my own api guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// qa-fixer: the skill directory is a symlink to somewhere else.
	elsewhere := t.TempDir()
	sentinel := filepath.Join(elsewhere, "SKILL.md")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".claude", "skills", "qa-fixer")); err != nil {
		t.Fatal(err)
	}
	// adversarial-qa: the user's identical unmarked copy, which orch adopts.
	qaPath := install.ClaudeAssetPath(home, qa.Main().Rel)
	user := strings.Replace(string(qa.Main().Body), "<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->\n\n", "", 1)
	if err := os.MkdirAll(filepath.Dir(qaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qaPath, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	all := func(string, string) (bool, bool) { return false, false }
	c, problems := p.Enforce(files, all)
	if _, wrote := c.Write[apiPath]; wrote {
		t.Error("the user's own api-design was overwritten")
	}
	if problems == nil || !strings.Contains(problems.Error(), apiPath) || !strings.Contains(problems.Error(), "qa-fixer") {
		t.Errorf("problems = %v; want the user file and the symlinked skill named", problems)
	}
	for _, f := range fixer.Files {
		if _, wrote := c.Write[install.ClaudeAssetPath(home, f.Rel)]; wrote {
			t.Errorf("%s would be written through a symlinked skill directory", f.Rel)
		}
	}
	if string(c.Write[qaPath]) != string(qa.Main().Body) {
		t.Error("the identical copy was not adopted")
	}

	// Everything off: the user's file and the linked skill are not deleted.
	off := func(string, string) (bool, bool) { return false, true }
	c, _ = p.Enforce(files, off)
	for _, d := range c.Delete {
		if d == apiPath || strings.HasPrefix(d, filepath.Join(home, ".claude", "skills", "qa-fixer")) {
			t.Errorf("would delete %s, which is not orch's", d)
		}
	}
	if b, _ := os.ReadFile(sentinel); string(b) != "keep me\n" {
		t.Error("sentinel changed")
	}
}

// TestEnforceAssetsAuditRegressions covers the two security audit findings:
// a symlinked skill directory is never pruned, and the pre-authorized
// exploit-finder wrapper is pinned to the shipped copy.
func TestEnforceAssetsAuditRegressions(t *testing.T) {
	home, p := assetHome(t)
	ef := findAsset(t, install.KindAgent, "exploit-finder")
	var wrapper install.AssetFile
	for _, f := range ef.Files {
		if f.Pinned {
			wrapper = f
		}
	}
	if wrapper.Rel != "agents/exploit-finder-sandbox.sh" {
		t.Fatalf("sandbox wrapper not pinned: %+v", ef.Files)
	}
	wrapperPath := install.ClaudeAssetPath(home, wrapper.Rel)
	if err := os.MkdirAll(filepath.Dir(wrapperPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// An unmarked replacement is not "the user's": on, it is restored.
	if err := os.WriteFile(wrapperPath, []byte("#!/bin/sh\nexec bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The user's dotfiles link api-design's skill directory in.
	dotfiles := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", "skills", "api-design")
	if err := os.Symlink(dotfiles, link); err != nil {
		t.Fatal(err)
	}
	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	on := func(string, string) (bool, bool) { return true, true }
	c, _ := p.Enforce(files, on)
	if string(c.Write[wrapperPath]) != string(wrapper.Body) {
		t.Error("a replaced sandbox wrapper was not restored")
	}

	// Off: anything at the wrapper path is removed, and the linked skill
	// directory is not pruned.
	if err := os.WriteFile(wrapperPath, []byte("planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _ = p.Read()
	off := func(string, string) (bool, bool) { return false, true }
	c, _ = p.Enforce(files, off)
	found := false
	for _, d := range c.Delete {
		found = found || d == wrapperPath
	}
	if !found {
		t.Error("a planted file at the pre-authorized wrapper path was left in place")
	}
	for _, d := range c.Prune {
		if d == link {
			t.Fatal("a symlinked skill directory was queued for removal")
		}
	}
	reg, _ := settings.NewRegistry(p)
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	svc := settings.NewService(settings.NewStore(conn), reg, nil)
	ctx := context.Background()
	svc.Sync(ctx)
	if _, err := svc.SetEnabled(ctx, install.InstructionsProvider, install.KindSkill, "api-design", false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the user's symlinked skill directory was removed: %v", err)
	}
	if l, _ := svc.List(ctx, install.InstructionsProvider, install.KindAgent); !hasSensitive(l.Rows, "security-audit") {
		t.Error("security-audit should be listed as sensitive")
	}
}

func hasSensitive(rows []settings.Row, name string) bool {
	for _, r := range rows {
		var p Payload
		if r.Identity == name && json.Unmarshal(r.Payload, &p) == nil {
			return p.Sensitive
		}
	}
	return false
}
