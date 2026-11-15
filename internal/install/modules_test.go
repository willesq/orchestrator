package install

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/willesq/orchestrator/internal/settings"
)

func TestModulesParse(t *testing.T) {
	mods, err := Modules()
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) == 0 {
		t.Fatal("no instruction modules embedded")
	}
	orders := map[int]string{}
	for _, m := range mods {
		if prev, dup := orders[m.Order]; dup {
			t.Errorf("%s and %s share order %d, so their import order is by name only", prev, m.Slug, m.Order)
		}
		orders[m.Order] = m.Slug
		if m.Group == "" || m.Description == "" {
			t.Errorf("%s needs a group and a description for the settings UI", m.Slug)
		}
		if strings.HasPrefix(m.Body, "---") {
			t.Errorf("%s: frontmatter leaked into the installed body", m.Slug)
		}
		// Reason: modules ship in the binary; a home path would be one
		// machine's and leak a username.
		if strings.Contains(m.Body, "/Users/") || strings.Contains(m.Body, "/home/") {
			t.Errorf("%s hard-codes a home directory; use ~ or $HOME", m.Slug)
		}
	}
}

func TestParseModuleErrors(t *testing.T) {
	ok := "---\ntitle: T\ngroup: G\norder: 1\ndescription: D\n---\n\nBody\n"
	m, err := parseModule("good-1", []byte(ok))
	if err != nil || m.Body != "Body\n" || !m.Default || m.Title != "T" {
		t.Fatalf("parse ok = %+v, %v", m, err)
	}
	for name, tc := range map[string]struct{ slug, raw string }{
		"bad slug":       {"Bad_Slug", ok},
		"no frontmatter": {"x", "Body\n"},
		"unterminated":   {"x", "---\ntitle: T\nBody\n"},
		"unknown key":    {"x", "---\ntitle: T\ncolour: red\n---\nBody\n"},
		"bad yaml":       {"x", "---\ntitle: a: b\n---\nBody\n"},
		"no title":       {"x", "---\ngroup: G\n---\nBody\n"},
		"empty body":     {"x", "---\ntitle: T\n---\n\n"},
	} {
		if _, err := parseModule(tc.slug, []byte(tc.raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	off, err := parseModule("x", []byte("---\ntitle: T\ndefault: false\n---\nBody\n"))
	if err != nil || off.Default {
		t.Errorf("default: false not honored: %+v %v", off, err)
	}
}

func TestParseModulesOrdersAndReportsFile(t *testing.T) {
	fsys := fstest.MapFS{
		"modules/b.md": {Data: []byte("---\ntitle: B\norder: 2\n---\nB\n")},
		"modules/a.md": {Data: []byte("---\ntitle: A\norder: 2\n---\nA\n")},
		"modules/c.md": {Data: []byte("---\ntitle: C\norder: 1\n---\nC\n")},
	}
	mods, err := parseModules(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mods {
		got = append(got, m.Slug)
	}
	if strings.Join(got, ",") != "c,a,b" {
		t.Errorf("order = %v, want by order then name", got)
	}
	fsys["modules/bad.md"] = &fstest.MapFile{Data: []byte("nope")}
	if _, err := parseModules(fsys); err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestModuleOn(t *testing.T) {
	mods, _ := Modules()
	m := mods[0]
	if !ModuleOn(settings.NewToggles(nil))(m) {
		t.Error("an untoggled module should follow its default (on)")
	}
	off := settings.Row{Provider: InstructionsProvider, Kind: KindModule, Identity: m.Slug, Present: true}
	if ModuleOn(settings.NewToggles([]settings.Row{off}))(m) {
		t.Error("a module turned off stayed on")
	}
	m.Default = false
	on := off
	on.Enabled = true
	if !ModuleOn(settings.NewToggles([]settings.Row{on}))(m) {
		t.Error("a default-off module turned on stayed off")
	}
}

func TestBridgeBodyListsOnlyEnabledModules(t *testing.T) {
	home := "/h ome"
	mods, _ := Modules()
	skip := mods[1].Slug
	on := func(m Module) bool { return m.Slug != skip }
	claude, err := bridgeBody(home, "claude-code", on)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(claude), "\n")
	if lines[0] != `@/h\ ome/.config/orch/AGENTS.md` || len(lines) != len(mods) {
		t.Fatalf("claude block = %q", claude)
	}
	if strings.Contains(claude, "/"+skip+".md") {
		t.Errorf("disabled module %s still imported", skip)
	}
	if lines[1] != "@"+escapeImport(ModulePath(home, mods[0].Slug)) {
		t.Errorf("modules not in order: %q", lines[1])
	}
	codex, _ := bridgeBody(home, "codex", on)
	if strings.Contains(codex, "\n@") || !strings.Contains(codex, "- "+ModulePath(home, mods[0].Slug)+"\n") ||
		strings.Contains(codex, skip+".md") {
		t.Errorf("codex block = %q", codex)
	}
	none, _ := bridgeBody(home, "codex", func(Module) bool { return false })
	if strings.Contains(none, "instruction modules") {
		t.Errorf("codex block with nothing on should not announce modules: %q", none)
	}
}

func TestInstallWritesModulesAndHonorsToggles(t *testing.T) {
	home := t.TempDir()
	mods, _ := Modules()
	off := mods[2].Slug
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return settings.NewToggles([]settings.Row{{Provider: InstructionsProvider, Kind: KindModule, Identity: off, Present: true}}), nil
	})()
	plan := InstallPlan("codex", instructionUnits(home, "codex"))
	if err := plan.Apply(io.Discard, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		b, err := os.ReadFile(ModulePath(home, m.Slug))
		if err != nil || string(b) != m.Body {
			t.Errorf("module %s not installed verbatim (err %v)", m.Slug, err)
		}
	}
	entry, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	if strings.Contains(string(entry), off+".md") || !strings.Contains(string(entry), mods[0].Slug+".md") {
		t.Errorf("codex entry ignores toggles:\n%s", entry)
	}
	fps := InstructionFootprints(home)
	if len(fps) != 1 || len(fps[0].Files) != len(mods)+1 {
		t.Errorf("codex footprint should count its entry, the shared policy and enabled modules: %+v", fps)
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Errorf("re-plan: %d pending, %v", n, err)
	}
	defer SetToggleLoader(func() (settings.Toggles, error) { return settings.Toggles{}, os.ErrPermission })()
	if _, err := plan.Pending(); err == nil {
		t.Error("an unreadable toggle store should stop the plan, not re-enable modules")
	}
}

func TestSensitiveModulesOff(t *testing.T) {
	mods, _ := Modules()
	var sensitive []string
	for _, m := range mods {
		if m.Sensitive {
			sensitive = append(sensitive, m.Slug)
		}
	}
	if strings.Join(sensitive, ",") != "security,security-audit-agent,credential-rules" {
		t.Errorf("sensitive modules = %v", sensitive)
	}
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return settings.NewToggles([]settings.Row{
			{Provider: InstructionsProvider, Kind: KindModule, Identity: "credential-rules", Present: true},
			{Provider: InstructionsProvider, Kind: KindModule, Identity: "gpg-signing", Present: true},
		}), nil
	})()
	off, err := SensitiveModulesOff()
	if err != nil || strings.Join(off, ",") != "credential-rules" {
		t.Errorf("off = %v, %v; want only the sensitive one", off, err)
	}
	defer SetToggleLoader(func() (settings.Toggles, error) { return settings.Toggles{}, os.ErrPermission })()
	if _, err := SensitiveModulesOff(); err == nil {
		t.Error("an unreadable store was hidden")
	}
}

func TestCodexFootprintIgnoresEscapingModuleLines(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, "elsewhere.md")
	write(t, secret, strings.Repeat("x", 4000))
	entry := filepath.Join(home, ".codex", "AGENTS.md")
	write(t, entry, "- "+ModuleDir(home)+"/../../../elsewhere.md\n- "+ModulePath(home, "missing")+"\n")
	fps := InstructionFootprints(home)
	if len(fps) != 1 || len(fps[0].Files) != 1 {
		t.Errorf("a module line outside the module dir was counted: %+v", fps)
	}
}
