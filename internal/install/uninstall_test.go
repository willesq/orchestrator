package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testHome is a temporary home holding the user's own content in every file
// install edits, each in orch's encoding, so a round trip can be compared
// byte for byte.
type testHome struct {
	home, data string
}

func mustJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestHome(t *testing.T) testHome {
	t.Helper()
	h := testHome{home: t.TempDir()}
	h.data = filepath.Join(h.home, ".local", "share", "orch")
	settings := mustJSON(t, map[string]any{
		"model": "x",
		"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "other-tool stop"}}}}},
		// The credential denies are already there: uninstall keeps them
		// (TestUninstallKeepsCredentialDenies), so a byte-for-byte round
		// trip starts from a file that has them.
		"permissions": map[string]any{"allow": []any{"Bash(ls:*)"},
			"deny": append([]any{"Read(~/.ssh/**)"}, anySlice(credentialReadDeny)...)},
		"attribution": map[string]any{"pr": "mine"},
		"mcpServers":  map[string]any{"serena": map[string]any{"command": "serena"}},
	})
	toml, err := marshalTOML(map[string]any{"model": "o", "mcp_servers": map[string]any{"other": map[string]any{"command": "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".claude/settings.json":              settings,
		".claude.json":                       mustJSON(t, map[string]any{"mcpServers": map[string]any{"serena": map[string]any{"command": "s"}}, "numStartups": 3}),
		".claude/CLAUDE.md":                  "# Mine\n\nKeep this.\n",
		".claude/agents/mine.md":             "my own agent, no marker\n",
		".codex/config.toml":                 string(toml),
		".codex/AGENTS.md":                   "codex rules\n",
		".gemini/settings.json":              mustJSON(t, map[string]any{"theme": "dark"}),
		".zprofile":                          "export A=1\n",
		".cc-safety-net/rules/rule.json":     mustJSON(t, map[string]any{"version": 1, "rules": []any{"mine"}}),
		".cc-safety-net/rules/mine/rulebook": "{}\n",
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(h.home, rel), body)
	}
	return h
}

// tree is every file under the home, less what is not orch's to restore:
// Claude Code's own plugin records (written by its CLI) and orch's backups.
func (h testHome) tree(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	skip := []string{filepath.Join(h.home, ".claude", "plugins"), filepath.Join(h.data, "backups")}
	err := filepath.WalkDir(h.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, s := range skip {
			if p == s {
				return filepath.SkipDir
			}
		}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[strings.TrimPrefix(p, h.home)] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeRun records commands instead of running launchctl or brew.
type fakeRun struct {
	calls []string
	fail  map[string]error
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) (string, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	for prefix, err := range f.fail {
		if strings.HasPrefix(c, prefix) {
			return "", err
		}
	}
	return "ok", nil
}

func (h testHome) machine(run *fakeRun) Machine {
	return Machine{Home: h.home, DataDir: h.data, ConfigDir: filepath.Join(h.home, ".config", "orch"),
		Relocated: true, Bins: testBins(), Run: run.run}
}

func (h testHome) units(adapters ...Adapter) []Unit {
	return FullUnits(h.machine(&fakeRun{}), adapters)
}

func applyPlan(t *testing.T, p Plan, h testHome) string {
	t.Helper()
	var out bytes.Buffer
	if err := p.Apply(&out, filepath.Join(h.data, "backups")); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	return out.String()
}

func diffTrees(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: missing after the round trip", k)
		} else if g != v {
			t.Errorf("%s: not restored\nwant %q\ngot  %q", k, v, g)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: left behind", k)
		}
	}
}

func TestEveryRegisteredUnitIsPaired(t *testing.T) {
	h := newTestHome(t)
	units := h.units(AllAdapters()...)
	if err := ValidateUnits(units); err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		for _, e := range u.Edits {
			if e.Reverse == nil {
				t.Errorf("unit %s: %s has no uninstall", u.Name, e.Path)
			}
		}
	}
}

func TestValidateUnitsRefusesUnpaired(t *testing.T) {
	write := func([]byte) ([]byte, string, error) { return []byte("x"), "x", nil }
	tests := []struct {
		name string
		unit Unit
	}{
		{"edit without reverse", Unit{Name: "a", Edits: []Edit{{Path: "/p", Compute: write}}}},
		{"steps without undo steps", Unit{Name: "b", Steps: func() []Step { return nil }}},
		{"installs nothing, says nothing", Unit{Name: "c", Leftovers: func() []string { return nil }}},
		{"no uninstall", Unit{Name: "d", InstalledBy: "someone"}},
		{"no name", Unit{InstalledBy: "x", Leftovers: func() []string { return nil }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateUnits([]Unit{tt.unit}); err == nil {
				t.Fatal("unpaired unit accepted")
			}
			home := t.TempDir()
			p := InstallPlan("x", []Unit{tt.unit})
			if err := p.Print(io.Discard); err == nil {
				t.Fatal("plan of an unpaired unit printed")
			}
			if err := p.Apply(io.Discard, home); err == nil {
				t.Fatal("plan of an unpaired unit applied")
			}
			if err := UninstallPlan("x", []Unit{tt.unit}, true).Apply(io.Discard, home); err == nil {
				t.Fatal("uninstall plan of an unpaired unit applied")
			}
		})
	}
}

func TestInstallUninstallRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		adapters []Adapter
	}{
		{"every harness", AllAdapters()},
		{"claude code", []Adapter{ClaudeCode{}}},
		{"codex", []Adapter{Codex{}}},
		{"gemini", []Adapter{Gemini{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHome(t)
			before := h.tree(t)
			units := h.units(tt.adapters...)
			applyPlan(t, InstallPlan("all", units), h)
			if n, err := InstallPlan("all", units).Pending(); err != nil || n != 0 {
				t.Fatalf("install did not converge: %d pending, %v", n, err)
			}
			if got := h.tree(t); len(got) <= len(before) {
				t.Fatal("install wrote nothing")
			}
			applyPlan(t, UninstallPlan("all", units, true), h)
			diffTrees(t, before, h.tree(t))
			for _, d := range []string{".config/orch", ".local/share/orch/marketplaces", ".cc-safety-net/rules/orch-guards"} {
				if _, err := os.Lstat(filepath.Join(h.home, d)); err == nil {
					t.Errorf("%s: orch-only directory left behind", d)
				}
			}
		})
	}
}

func TestOneCommandInstallMatchesPerTargetInstalls(t *testing.T) {
	combined, separate := newTestHome(t), newTestHome(t)
	applyPlan(t, InstallPlan("all", combined.units(AllAdapters()...)), combined)
	for _, a := range AllAdapters() {
		applyPlan(t, BuildPlan(a, testBins(), separate.home), separate)
	}
	got, want := combined.tree(t), separate.tree(t)
	rel := func(m map[string]string, home string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = strings.ReplaceAll(v, home, "<home>")
		}
		return out
	}
	diffTrees(t, rel(want, separate.home), rel(got, combined.home))
}

func TestUninstallRepeatAndDryRunChangeNothing(t *testing.T) {
	h := newTestHome(t)
	units := h.units(AllAdapters()...)
	applyPlan(t, InstallPlan("all", units), h)

	installed := h.tree(t)
	var dry bytes.Buffer
	if err := UninstallPlan("all", units, true).Print(&dry); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "DRY RUN") || !strings.Contains(dry.String(), "REMOVE") {
		t.Fatalf("dry run lists no removals:\n%s", dry.String())
	}
	diffTrees(t, installed, h.tree(t))

	applyPlan(t, UninstallPlan("all", units, true), h)
	after := h.tree(t)
	if n, err := UninstallPlan("all", units, true).Pending(); err != nil || n != 0 {
		t.Fatalf("second uninstall has %d pending, %v", n, err)
	}
	out := applyPlan(t, UninstallPlan("all", units, true), h)
	if !strings.Contains(out, "nothing changed") {
		t.Errorf("second uninstall output:\n%s", out)
	}
	diffTrees(t, after, h.tree(t))
}

func TestUninstallPreservesUserContent(t *testing.T) {
	h := newTestHome(t)
	units := h.units(AllAdapters()...)
	applyPlan(t, InstallPlan("all", units), h)
	// Content the user adds after the install stays too.
	appendFile(t, filepath.Join(h.home, ".claude", "CLAUDE.md"), "Added later.\n")
	writeFile(t, filepath.Join(h.home, ".claude", "agents", "later.md"), "another of mine\n")
	settings := readJSON(t, filepath.Join(h.home, ".claude", "settings.json"))
	settings["mcpServers"].(map[string]any)["rag"] = map[string]any{"command": "/elsewhere/other-rag"}
	writeFile(t, filepath.Join(h.home, ".claude", "settings.json"), mustJSON(t, settings))

	applyPlan(t, UninstallPlan("all", units, true), h)
	claudeMD := readFile(t, filepath.Join(h.home, ".claude", "CLAUDE.md"))
	if strings.Contains(claudeMD, instructionBegin) || !strings.Contains(claudeMD, "Keep this.") || !strings.Contains(claudeMD, "Added later.") {
		t.Errorf("CLAUDE.md = %q", claudeMD)
	}
	for _, f := range []string{"mine.md", "later.md"} {
		if _, err := os.Stat(filepath.Join(h.home, ".claude", "agents", f)); err != nil {
			t.Errorf("user agent %s removed: %v", f, err)
		}
	}
	got := readJSON(t, filepath.Join(h.home, ".claude", "settings.json"))
	if got["mcpServers"].(map[string]any)["rag"] == nil {
		t.Error("a rag server that is not orch's was removed")
	}
	if got["mcpServers"].(map[string]any)["queue"] != nil {
		t.Error("orch's queue server was left")
	}
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallPartialInstall(t *testing.T) {
	h := newTestHome(t)
	before := h.tree(t)
	units := h.units(AllAdapters()...)
	applyPlan(t, InstallPlan("all", units), h)
	// Pieces removed by hand, or never written, are simply not there.
	mods, err := Modules()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ModulePath(h.home, mods[0].Slug), filepath.Join(h.home, ".codex", "hooks.json"), SharedPolicyPath(h.home)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	applyPlan(t, UninstallPlan("all", units, true), h)
	diffTrees(t, before, h.tree(t))

	// An uninstall on a machine orch never touched changes nothing.
	fresh := newTestHome(t)
	untouched := fresh.tree(t)
	out := applyPlan(t, UninstallPlan("all", fresh.units(AllAdapters()...), true), fresh)
	if !strings.Contains(out, "nothing changed") {
		t.Errorf("uninstall on a clean home:\n%s", out)
	}
	diffTrees(t, untouched, fresh.tree(t))
}

func TestUninstallOneHarnessKeepsShared(t *testing.T) {
	h := newTestHome(t)
	applyPlan(t, InstallPlan("all", h.units(ClaudeCode{}, Codex{})), h)
	applyPlan(t, UninstallPlan("claude-code", ClaudeCode{}.Units(testBins(), h.home), false), h)
	if HasManagedBlock([]byte(readFile(t, filepath.Join(h.home, ".claude", "CLAUDE.md")))) {
		t.Error("claude block left")
	}
	if !HasManagedBlock([]byte(readFile(t, filepath.Join(h.home, ".codex", "AGENTS.md")))) {
		t.Error("codex block removed by a claude-code uninstall")
	}
	for _, p := range []string{SharedPolicyPath(h.home), PlistPath(h.home)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("shared %s removed by a single-harness uninstall", p)
		}
	}
}

func TestUninstallSymlinkedEntryFile(t *testing.T) {
	h := testHome{home: t.TempDir()}
	h.data = filepath.Join(h.home, ".local", "share", "orch")
	target := filepath.Join(h.home, "dotfiles", "GEMINI.md")
	writeFile(t, target, "")
	symlink(t, target, filepath.Join(h.home, ".gemini", "GEMINI.md"))
	units := h.units(Gemini{})
	applyPlan(t, InstallPlan("all", units), h)
	if !HasManagedBlock([]byte(readFile(t, target))) {
		t.Fatal("install did not write through the link")
	}
	applyPlan(t, UninstallPlan("all", units, true), h)
	if !isSymlink(t, filepath.Join(h.home, ".gemini", "GEMINI.md")) {
		t.Fatal("the symlink was removed")
	}
	if got := readFile(t, target); got != "" {
		t.Errorf("file behind the link = %q", got)
	}
}

func TestUninstallRefusesMalformedMarkers(t *testing.T) {
	h := newTestHome(t)
	units := h.units(ClaudeCode{})
	applyPlan(t, InstallPlan("all", units), h)
	md := filepath.Join(h.home, ".claude", "CLAUDE.md")
	writeFile(t, md, strings.Replace(readFile(t, md), instructionEnd, "", 1))
	before := h.tree(t)
	if err := UninstallPlan("all", units, true).Apply(io.Discard, filepath.Join(h.data, "backups")); err == nil {
		t.Fatal("broken markers accepted")
	}
	diffTrees(t, before, h.tree(t))
}

func TestUninstallBacksUpEveryFile(t *testing.T) {
	h := newTestHome(t)
	units := h.units(ClaudeCode{})
	applyPlan(t, InstallPlan("all", units), h)
	installed := readFile(t, filepath.Join(h.home, ".claude", "settings.json"))
	backups := filepath.Join(t.TempDir(), "b")
	if err := UninstallPlan("all", units, true).Apply(io.Discard, backups); err != nil {
		t.Fatal(err)
	}
	var found bool
	_ = filepath.WalkDir(backups, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && strings.HasSuffix(p, ".claude__settings.json") {
			b, _ := os.ReadFile(p)
			found = string(b) == installed
			info, _ := os.Stat(p)
			if info.Mode().Perm() != 0o600 {
				t.Errorf("backup mode %v", info.Mode().Perm())
			}
		}
		return nil
	})
	if !found {
		t.Error("settings.json was not backed up before uninstall changed it")
	}
}

func TestReverseHelpers(t *testing.T) {
	tests := []struct {
		name, in, want string
		remove         bool
	}{
		{"block appended to text", "a\n\n" + instructionBegin + "\nx\n" + instructionEnd + "\n", "a\n", false},
		{"block alone", "\n" + instructionBegin + "\nx\n" + instructionEnd + "\n", "", true},
		{"block first", instructionBegin + "\nx\n" + instructionEnd + "\nrest\n", "rest\n", false},
		{"block between lines", "a\n" + instructionBegin + "\nx\n" + instructionEnd + "\nb\n", "a\nb\n", false},
		{"no block", "a\n", "a\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := cutManagedInstructions([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if u.Remove != tt.remove || (!tt.remove && string(u.Data) != tt.want) {
				t.Errorf("got %+v", u)
			}
		})
	}
	if _, err := cutAliasBlock([]byte(aliasBegin + "\nalias orch=x\n")); err == nil {
		t.Error("alias block without end accepted")
	}
	if u, _ := cutAliasBlock([]byte(aliasBegin + "\nalias orch=x\n" + aliasEnd + "\n")); !u.Remove {
		t.Error("profile with only orch's block not removed")
	}
	if _, err := undoJSON([]byte("{"), "x", true, stripRules); err == nil {
		t.Error("broken JSON accepted")
	}
	if _, err := undoTOML([]byte("= ="), "x", stripRules); err == nil {
		t.Error("broken TOML accepted")
	}
	if u, _ := undoOwnedFile([]byte("x"), "x", IsManaged); u.Remove {
		t.Error("unmarked file removed")
	}
}

func TestMachineDaemonSteps(t *testing.T) {
	h := newTestHome(t)
	up := false
	tests := []struct {
		name               string
		homebrew, loaded   bool
		wantStart, wantOff string
	}{
		{"launchd", false, true, "launchctl load -w", "launchctl unload -w"},
		{"homebrew", true, true, "brew services start orch", "brew services stop orch"},
		{"launchd, not loaded", false, false, "launchctl load -w", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &fakeRun{fail: map[string]error{}}
			if !tt.loaded {
				run.fail["launchctl list"] = errors.New("not loaded")
			}
			m := h.machine(run)
			m.Relocated, m.Homebrew = false, tt.homebrew
			m.DaemonUp = func() bool { return up }
			writeFile(t, PlistPath(h.home), "plist")
			d := m.daemonUnit()
			start := d.Steps()
			if len(start) != 1 || !strings.Contains(start[0].Summary, tt.wantStart) {
				t.Fatalf("start steps = %+v", start)
			}
			if _, err := start[0].Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			up = tt.loaded
			stop := d.UndoSteps()
			if tt.wantOff == "" {
				if len(stop) != 0 {
					t.Fatalf("stop steps for an agent not loaded: %+v", stop)
				}
				return
			}
			if len(stop) != 1 || !strings.Contains(stop[0].Summary, tt.wantOff) {
				t.Fatalf("stop steps = %+v", stop)
			}
			up = true
			if len(d.Steps()) != 0 {
				t.Error("start planned while orchd is up")
			}
			up = false
		})
	}
	t.Run("relocated home never runs launchctl or brew", func(t *testing.T) {
		run := &fakeRun{}
		m := h.machine(run)
		if len(m.daemonUnit().Steps())+len(m.daemonUnit().UndoSteps()) != 0 || len(run.calls) != 0 {
			t.Fatalf("relocated home planned daemon steps: %v", run.calls)
		}
	})
}

func TestMachineModelStep(t *testing.T) {
	h := newTestHome(t)
	m := h.machine(&fakeRun{})
	present, fetched := false, 0
	m.ModelPresent = func() bool { return present }
	m.FetchModel = func(context.Context) error { fetched++; present = true; return nil }
	steps := m.modelUnit().Steps()
	if len(steps) != 1 {
		t.Fatalf("steps = %d", len(steps))
	}
	if _, err := steps[0].Run(context.Background()); err != nil || fetched != 1 {
		t.Fatal(err)
	}
	if len(m.modelUnit().Steps()) != 0 {
		t.Error("model planned again once present")
	}
	m.NoModel, present = true, false
	if len(m.modelUnit().Steps()) != 0 {
		t.Error("--no-model planned a download")
	}
	if len(m.modelUnit().UndoSteps()) != 0 {
		t.Error("uninstall deletes the model without --purge-data")
	}
}

func TestMachineAppAndNotes(t *testing.T) {
	h := newTestHome(t)
	m := h.machine(&fakeRun{})
	app := filepath.Join(h.home, "Applications", "Orch.app")
	m.AppPaths = []string{app}
	if got := m.appUnit().Manual(); len(got) != 1 {
		t.Errorf("no cask hint without the app: %v", got)
	}
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := m.appUnit().Leftovers(); len(got) != 1 || !strings.Contains(got[0], "Login Items") {
		t.Errorf("leftovers = %v", got)
	}
	var out bytes.Buffer
	if err := UninstallPlan("all", FullUnits(m, nil), true).Print(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Left for you", "Orch.app", "--purge-data", "orch guard git-hook --uninstall"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("uninstall notes lack %q:\n%s", want, out.String())
		}
	}
}

func TestDeepestFirst(t *testing.T) {
	got := deepestFirst([]string{"/a", "/a/b/c", "/a/b", "/a/b"})
	want := []string{"/a/b/c", "/a/b", "/a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v", got)
	}
}

func TestRemoveRegularRefusesNonFiles(t *testing.T) {
	dir := t.TempDir()
	if err := removeRegular(dir); err == nil {
		t.Error("directory removed")
	}
	if err := removeRegular(filepath.Join(dir, "missing")); err != nil {
		t.Error(err)
	}
}

func TestEvaluateUndoRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "d")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	e := Edit{Path: sub, undo: true, Reverse: func([]byte) (Undo, error) { return removeUndo("x"), nil }}
	if _, err := e.evaluate(); err == nil {
		t.Error("removal of a directory planned")
	}
	e.Reverse = nil
	if _, err := e.evaluate(); err == nil {
		t.Error("edit without reverse evaluated")
	}
	if _, err := (Edit{Path: sub}).evaluate(); err == nil {
		t.Error("edit without compute evaluated")
	}
}

func TestReverseReportsReadErrors(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	_ = enc.Encode(map[string]any{})
	u, err := undoJSON(buf.Bytes(), "x", true, stripHooks(""))
	if err != nil || u.Remove {
		t.Errorf("empty object with nothing of orch's: %+v %v", u, err)
	}
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestUninstallKeepsCredentialDenies(t *testing.T) {
	h := testHome{home: t.TempDir()}
	h.data = filepath.Join(h.home, ".local", "share", "orch")
	settings := filepath.Join(h.home, ".claude", "settings.json")
	writeFile(t, settings, mustJSON(t, map[string]any{"model": "x"}))
	units := h.units(ClaudeCode{})
	applyPlan(t, InstallPlan("all", units), h)
	out := applyPlan(t, UninstallPlan("all", units, true), h)
	want := mustJSON(t, map[string]any{"model": "x", "permissions": map[string]any{"deny": anySlice(credentialReadDeny)}})
	if got := readFile(t, settings); got != want {
		t.Errorf("settings.json = %s, want %s", got, want)
	}
	if !strings.Contains(out, "credential-file Read deny rules stay") {
		t.Errorf("uninstall does not name the kept deny rules:\n%s", out)
	}
}

func TestStripHooksIsStrict(t *testing.T) {
	entry := func(cmd string) any {
		return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}
	}
	root := map[string]any{"hooks": map[string]any{"Stop": []any{
		entry("/opt/orch/orch-hook stop"),
		entry("/elsewhere/orch-hook stop"),
		entry("python3 /u/.claude/orchestrator/hooks/stop.py"),
		entry("~/bin/my-orch-hook-wrapper.sh stop"),
		entry("echo orch-hook"),
	}}}
	stripHooks("/opt/orch/orch-hook")(root)
	left := root["hooks"].(map[string]any)["Stop"].([]any)
	if len(left) != 2 {
		t.Fatalf("left %v", left)
	}
}
