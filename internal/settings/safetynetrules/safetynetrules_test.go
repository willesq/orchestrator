package safetynetrules

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*Provider, safetynet.Locations) {
	t.Helper()
	loc := safetynet.Locate(t.TempDir())
	writeFile(t, loc.RulebookPath(safetynet.RulebookName), string(safetynet.Rulebook()))
	writeFile(t, loc.RulebookPath("mine"), `{"rulebook_version":1,"name":"mine","version":"1","rules":[
		{"name":"no-push","command":"git","subcommand":"push","block_args":["--force"],"reason":"r"},
		{"name":"no-push","command":"git","reason":"duplicate"},
		{"name":"bad name!","command":"x","reason":"r"}]}`)
	writeFile(t, loc.RulebookPath("v2"), `{"rulebook_version":2,"name":"v2","version":"1",
		"rules":[{"name":"c","command":"aws","match":{"command_path":["s3","rm"],"any_args":["--recursive"]},"reason":"r"}]}`)
	writeFile(t, loc.RulebookPath("broken"), `{"rulebook_version":9,"name":"broken","rules":[{"name":"x"}]}`)
	writeFile(t, loc.RuleConfigPath(), `{"$schema":"s","version":1,
		"rules":["orch-guards","mine","v2","o/r#main/team","broken","../escape","missing"],
		"overrides":{"mine/no-push":"off"},"transparent_wrappers":["rtk"]}`)
	return New(loc), loc
}

func items(t *testing.T, p *Provider) map[string]settings.Item {
	t.Helper()
	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	list, err := p.Observe(files)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]settings.Item{}
	for _, it := range list {
		out[it.Identity] = it
	}
	return out
}

func TestObserve(t *testing.T) {
	p, _ := setup(t)
	if p.Name() != install.SafetyNetRulesProvider || p.Title() == "" || len(p.Kinds()) != 1 || !p.Kinds()[0].StateInFile {
		t.Fatalf("provider metadata")
	}
	got := items(t, p)
	if it := got["orch-guards/gh-auth-token"]; it.Origin != settings.OriginOrch || !it.Enabled {
		t.Errorf("orch rule = %+v", it)
	}
	if it := got["mine/no-push"]; it.Origin != settings.OriginFound || it.Enabled {
		t.Errorf("overridden rule = %+v", it)
	}
	if _, ok := got["v2/c"]; !ok {
		t.Error("v2 rulebook rule missing")
	}
	for id := range got {
		if strings.HasPrefix(id, "broken/") || strings.Contains(id, "bad name") || strings.HasPrefix(id, "team/") {
			t.Errorf("%s listed from a rulebook cc-safety-net skips", id)
		}
	}
	g, s := p.Describe(KindRule, got["mine/no-push"].Payload)
	if g != "mine" || s != "no-push: git push --force" {
		t.Errorf("describe = %q %q", g, s)
	}
	if g, s := p.Describe(KindRule, []byte("{")); g != "" || s != "" {
		t.Error("bad payload described")
	}
	if _, err := p.Validate(KindRule, nil); !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("validate: %v", err)
	}
}

func TestObserveEdges(t *testing.T) {
	loc := safetynet.Locate(t.TempDir())
	p := New(loc)
	if got := items(t, p); len(got) != 0 {
		t.Errorf("no rule.json: %v", got)
	}
	writeFile(t, loc.RuleConfigPath(), `{`)
	files, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Observe(files); err == nil {
		t.Error("parse error not reported")
	}
	writeFile(t, loc.RuleConfigPath(), `{"version":1,"rules":["dir"]}`)
	if err := os.MkdirAll(loc.RulebookPath("dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read(); err == nil {
		t.Error("a rulebook that is a directory read")
	}
	if err := os.MkdirAll(loc.RuleConfigPath()+".d", 0o755); err != nil {
		t.Fatal(err)
	}
	bad := New(safetynet.Locations{CCSNHome: filepath.Join(loc.RuleConfigPath(), "x")})
	if _, err := bad.Read(); err == nil {
		t.Error("rule.json under a file read")
	}
}

func TestApply(t *testing.T) {
	p, loc := setup(t)
	files, _ := p.Read()
	on := settings.Item{Kind: KindRule, Identity: "mine/no-push"}
	out, err := p.Apply(files, on, true)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out[loc.RuleConfigPath()])
	if strings.Contains(doc, "mine/no-push") || !strings.Contains(doc, `"$schema": "s"`) || !strings.Contains(doc, `"rtk"`) {
		t.Errorf("enable:\n%s", doc)
	}
	off := settings.Item{Kind: KindRule, Identity: "orch-guards/gh-auth-token"}
	out, err = p.Apply(files, off, false)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	_ = json.Unmarshal(out[loc.RuleConfigPath()], &root)
	if ov := root["overrides"].(map[string]any); ov["orch-guards/gh-auth-token"] != "off" || ov["mine/no-push"] != "off" {
		t.Errorf("overrides = %v", ov)
	}
	if out, err := p.Apply(files, settings.Item{Kind: KindRule, Identity: "mine/no-push"}, false); out != nil || err != nil {
		t.Errorf("already off = %v %v", out, err)
	}
	for name, tt := range map[string]struct {
		doc string
		it  settings.Item
	}{
		"traversal":         {`{"version":1}`, settings.Item{Kind: KindRule, Identity: "../x/y"}},
		"no slash":          {`{"version":1}`, settings.Item{Kind: KindRule, Identity: "x"}},
		"wrong kind":        {`{"version":1}`, settings.Item{Kind: "hook", Identity: "a/b"}},
		"version 2":         {`{"version":2}`, settings.Item{Kind: KindRule, Identity: "a/b"}},
		"overrides array":   {`{"version":1,"overrides":[]}`, settings.Item{Kind: KindRule, Identity: "a/b"}},
		"unparseable":       {`{`, settings.Item{Kind: KindRule, Identity: "a/b"}},
		"version as string": {`{"version":"1"}`, settings.Item{Kind: KindRule, Identity: "a/b"}},
	} {
		if _, err := p.Apply(map[string][]byte{loc.RuleConfigPath(): []byte(tt.doc)}, tt.it, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No overrides block yet: one is created.
	out, err = p.Apply(map[string][]byte{loc.RuleConfigPath(): []byte(`{"version":1}`)}, settings.Item{Kind: KindRule, Identity: "a/b"}, false)
	if err != nil || !strings.Contains(string(out[loc.RuleConfigPath()]), `"a/b": "off"`) {
		t.Errorf("create overrides: %s %v", out[loc.RuleConfigPath()], err)
	}
}

// TestServiceToggleKeepsInstallMerge checks the toggle survives orch
// install's rule.json merge, which preserves overrides.
func TestServiceToggleKeepsInstallMerge(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p, loc := setup(t)
	reg, _ := settings.NewRegistry(p)
	svc := settings.NewService(settings.NewStore(conn), reg, nil)
	ctx := context.Background()
	if r, err := svc.SetEnabled(ctx, p.Name(), KindRule, "orch-guards/gh-auth-token", false); err != nil || r.Status() != settings.StatusDisabled {
		t.Fatalf("disable: %+v %v", r, err)
	}
	raw, _ := os.ReadFile(loc.RuleConfigPath())
	merged, _, err := safetynet.MergeRuleConfig(raw)
	if err != nil || !strings.Contains(string(merged), `"orch-guards/gh-auth-token": "off"`) {
		t.Errorf("install's merge dropped the override: %v\n%s", err, merged)
	}
	// Removing the rule from its rulebook marks it removed.
	writeFile(t, loc.RulebookPath("mine"), `{"rulebook_version":1,"name":"mine","version":"1","rules":[]}`)
	l, err := svc.List(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Rows {
		if r.Identity == "mine/no-push" && r.Status() != settings.StatusRemoved {
			t.Errorf("rule gone from its rulebook = %s", r.Status())
		}
	}
	if _, err := svc.SetEnabled(ctx, p.Name(), KindRule, "mine/no-push", true); !errors.Is(err, settings.ErrNotAllowed) {
		t.Errorf("toggling a vanished rule: %v", err)
	}
	if err := svc.Remove(ctx, p.Name(), KindRule, "mine/no-push"); err != nil {
		t.Errorf("forgetting a vanished rule: %v", err)
	}
	if err := svc.Remove(ctx, p.Name(), KindRule, "v2/c"); !errors.Is(err, settings.ErrNotAllowed) {
		t.Errorf("rules are not removable: %v", err)
	}
}
