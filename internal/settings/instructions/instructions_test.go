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

	l, err := svc.List(ctx, install.InstructionsProvider, "")
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
	if changed, err := p.Enforce(files, none); err != nil || len(changed) != 0 {
		t.Errorf("an unwired home was written: %d files, %v", len(changed), err)
	}

	codex := filepath.Join(home, ".codex", "AGENTS.md")
	files[codex] = []byte("Mine\n<!-- BEGIN orchestrator-managed -->\nold\n<!-- END orchestrator-managed -->\n")
	off := mods[0].Slug
	changed, err := p.Enforce(files, func(kind, id string) (bool, bool) {
		return id != off, id == off
	})
	if err != nil {
		t.Fatal(err)
	}
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
	changed, err = p.Enforce(files, none)
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
	changed, problems := p.Enforce(files, func(string, string) (bool, bool) { return false, false })
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
	if p.Name() != install.InstructionsProvider || p.Title() == "" || len(p.Kinds()) != 1 || !p.Kinds()[0].StateInFile {
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
