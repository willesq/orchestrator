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

func marked(body string) []byte {
	if strings.HasPrefix(body, "---\n") {
		end := strings.Index(body[4:], "\n---\n") + 4 + 5
		return []byte(body[:end] + markerInsert + body[end:])
	}
	return []byte(markerInsert + body)
}

func TestClaudeAssetsCatalog(t *testing.T) {
	list, err := ClaudeAssets()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ClaudeAsset{}
	for _, a := range list {
		got[a.Kind+"/"+a.Name] = a
	}
	for _, want := range []string{
		"agent/adversarial-qa", "agent/adversarial-reviewer", "agent/documentation-manager", "agent/feature-coder",
		"agent/initializer", "agent/security-audit", "agent/state-recovery", "agent/validation",
		"agent/dev", "agent/exploit-finder", "skill/adversarial-qa", "skill/api-design", "skill/qa-fixer", "skill/orch-dispatch",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s not shipped", want)
		}
	}
	for _, excluded := range []string{"skill/code-reviewer", "skill/swift-expert", "skill/macos-design-guidelines",
		"skill/asc-notarization", "skill/build-macos-apps", "skill/macos-accessibility"} {
		if _, ok := got[excluded]; ok {
			t.Errorf("%s ships, but it is third-party, unknown-origin or work-specific", excluded)
		}
	}
	qa := got["agent/adversarial-qa"]
	if len(qa.Files) != 5 || qa.Main().Rel != "agents/adversarial-qa.md" ||
		strings.Join(qa.Dirs, ",") != "agents/_shared/adversarial-qa-references" {
		t.Errorf("adversarial-qa = %d files, main %s, dirs %v; want its _shared references", len(qa.Files), qa.Main().Rel, qa.Dirs)
	}
	if ef := got["agent/exploit-finder"]; len(ef.Files) != 2 || ef.Files[1].Rel != "agents/exploit-finder-sandbox.sh" {
		t.Errorf("exploit-finder should own its sandbox helper: %+v", ef.Files)
	}
	fixer := got["skill/qa-fixer"]
	if fixer.Main().Rel != "skills/qa-fixer/SKILL.md" || len(fixer.Files) != 5 ||
		strings.Join(fixer.Dirs, ",") != "skills/qa-fixer/references,skills/qa-fixer" {
		t.Errorf("qa-fixer = %+v dirs %v", fixer.Main().Rel, fixer.Dirs)
	}
	if fixer.Title != "qa-fixer" || fixer.Description == "" || strings.Contains(fixer.Description, "\n") {
		t.Errorf("qa-fixer title/description = %q / %q", fixer.Title, fixer.Description)
	}
	if list[0].Kind != KindAgent || list[len(list)-1].Kind != KindSkill {
		t.Error("agents should list before skills")
	}
}

func TestParseClaudeAssetsRejectsStrays(t *testing.T) {
	m := string(markerInsert)
	ok := fstest.MapFS{
		"agents/a.md":       {Data: []byte("---\nname: a\n---\n" + m)},
		"agents/a-tool.sh":  {Data: []byte(m)},
		"skills/s/SKILL.md": {Data: []byte("---\nname: s\n---\n" + m)},
	}
	if _, err := parseClaudeAssets(ok); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string]fstest.MapFS{
		"unowned helper":     {"agents/b-tool.sh": {Data: []byte(m)}},
		"unowned shared":     {"agents/_shared/zz-refs/x.md": {Data: []byte(m)}},
		"stray top level":    {"README.md": {Data: []byte(m)}},
		"skill without main": {"skills/t/notes.md": {Data: []byte(m)}},
		"no marker":          {"skills/s/extra.md": {Data: []byte("plain")}},
	} {
		fsys := fstest.MapFS{}
		for k, v := range ok {
			fsys[k] = v
		}
		for k, v := range extra {
			fsys[k] = v
		}
		if _, err := parseClaudeAssets(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIsAdoptable(t *testing.T) {
	user := "---\nname: x\n---\n\n# Body\n"
	shipped := marked(user)
	if !IsAdoptable([]byte(user), shipped) {
		t.Error("the user's identical copy should be adoptable")
	}
	if IsAdoptable([]byte(user+"edited\n"), shipped) || IsAdoptable(nil, shipped) {
		t.Error("an edited or missing copy was adoptable")
	}
	if IsAdoptable([]byte(user), []byte(user)) {
		t.Error("a shipped body without the inserted marker cannot be adopted against")
	}
}

func TestInstallAdoptsAndHonorsAssetToggles(t *testing.T) {
	home := t.TempDir()
	list, _ := ClaudeAssets()
	var api, qa ClaudeAsset
	for _, a := range list {
		switch a.Kind + "/" + a.Name {
		case "skill/api-design":
			api = a
		case "agent/adversarial-qa":
			qa = a
		}
	}
	// The user's own, unmarked copy of api-design, byte for byte.
	apiPath := ClaudeAssetPath(home, api.Main().Rel)
	write(t, apiPath, string(bytesWithoutMarker(api.Main().Body)))
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return settings.NewToggles([]settings.Row{{Provider: InstructionsProvider, Kind: KindAgent, Identity: qa.Name, Present: true}}), nil
	})()
	plan := Plan{Target: "claude-code", Edits: claudeAssetEdits(home)}
	if err := plan.Apply(io.Discard, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(apiPath); string(b) != string(api.Main().Body) {
		t.Error("the identical copy was not taken over")
	}
	for _, f := range qa.Files {
		if _, err := os.Stat(ClaudeAssetPath(home, f.Rel)); !os.IsNotExist(err) {
			t.Errorf("%s installed although adversarial-qa is off", f.Rel)
		}
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Errorf("re-plan: %d pending, %v", n, err)
	}

	// A different, unmarked file is the user's: the plan refuses it.
	write(t, apiPath, "my own api-design\n")
	if _, err := plan.Pending(); err == nil || !strings.Contains(err.Error(), "yours") {
		t.Errorf("an edited unmarked copy was not refused: %v", err)
	}
	defer SetToggleLoader(func() (settings.Toggles, error) { return settings.Toggles{}, os.ErrPermission })()
	if _, err := plan.Pending(); err == nil {
		t.Error("an unreadable toggle store should stop the plan")
	}
}

func bytesWithoutMarker(b []byte) []byte {
	return []byte(strings.Replace(string(b), markerInsert, "", 1))
}

func TestDisplayBody(t *testing.T) {
	got := DisplayBody(marked("---\nname: x\ndescription: y\n---\n\n# Title\nbody\n"))
	if got != "# Title\nbody\n" {
		t.Errorf("DisplayBody = %q", got)
	}
	if got := DisplayBody([]byte("no frontmatter\n")); got != "no frontmatter\n" {
		t.Errorf("DisplayBody(plain) = %q", got)
	}
}

func TestFrontmatterNameDescLenient(t *testing.T) {
	name, desc := frontmatterNameDesc([]byte("---\nname: q\ndescription: Use when: things \"quoted\": happen\n---\nbody"), "fb")
	if name != "q" || desc != `Use when: things "quoted": happen` {
		t.Errorf("lenient parse = %q / %q", name, desc)
	}
	if name, _ := frontmatterNameDesc([]byte("body only"), "fb"); name != "fb" {
		t.Errorf("fallback name = %q", name)
	}
	if name, desc := frontmatterNameDesc([]byte("---\nname: f\ndescription: >\n  folded\n  text\n---\n"), "fb"); name != "f" || desc != "folded text" {
		t.Errorf("folded = %q / %q", name, desc)
	}
}

func TestParseClaudeAssetsSkipsDotFiles(t *testing.T) {
	m := string(markerInsert)
	fsys := fstest.MapFS{
		"agents/a.md":            {Data: []byte("---\nname: a\n---\n" + m)},
		"agents/.DS_Store":       {Data: []byte("junk")},
		"skills/s/SKILL.md":      {Data: []byte("---\nname: s\n---\n" + m)},
		"skills/s/.SKILL.md.swp": {Data: []byte("junk")},
		"skills/.cache/whatever": {Data: []byte("junk")},
	}
	list, err := parseClaudeAssets(fsys)
	if err != nil || len(list) != 2 {
		t.Fatalf("dot-files broke the catalog: %v %+v", err, list)
	}
}

func TestSandboxRuleFollowsExploitFinderToggle(t *testing.T) {
	root := map[string]any{"permissions": map[string]any{"allow": []any{exploitFinderSandboxRule, "Bash(ls:*)"}}}
	off := settings.NewToggles([]settings.Row{{Provider: InstructionsProvider, Kind: KindAgent, Identity: "exploit-finder", Present: true}})
	if _, err := applyPermissionsWith(root, off); err != nil {
		t.Fatal(err)
	}
	allow := root["permissions"].(map[string]any)["allow"].([]any)
	if hasRule(allow, exploitFinderSandboxRule) || !hasRule(allow, "Bash(ls:*)") {
		t.Errorf("with exploit-finder off: allow = %v", allow)
	}
	if _, err := applyPermissionsWith(root, settings.NewToggles(nil)); err != nil {
		t.Fatal(err)
	}
	if !hasRule(root["permissions"].(map[string]any)["allow"].([]any), exploitFinderSandboxRule) {
		t.Error("with exploit-finder on the rule was not added")
	}
}

func TestSensitiveAgentsOff(t *testing.T) {
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return settings.NewToggles([]settings.Row{{Provider: InstructionsProvider, Kind: KindAgent, Identity: "security-audit", Present: true}}), nil
	})()
	off, err := SensitiveModulesOff()
	if err != nil || strings.Join(off, ",") != "agent security-audit" {
		t.Errorf("off = %v, %v", off, err)
	}
}
