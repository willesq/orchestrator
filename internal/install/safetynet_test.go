package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
)

func TestApplyCredentialDeny(t *testing.T) {
	root := map[string]any{"permissions": map[string]any{
		"deny":  []any{"Bash(sudo *)", "Read(~/.netrc)"},
		"allow": []any{"Bash"},
	}}
	summary, err := applyCredentialDeny(root)
	if err != nil {
		t.Fatal(err)
	}
	deny := root["permissions"].(map[string]any)["deny"].([]any)
	want := []any{"Bash(sudo *)", "Read(~/.netrc)", "Read(~/.config/gh/**)", "Read(~/.docker/config.json)"}
	if !slices.Equal(deny, want) {
		t.Errorf("deny = %v, want %v", deny, want)
	}
	if !strings.Contains(summary, "added") || strings.Contains(summary, "Read(~/.netrc),") {
		t.Errorf("summary %q", summary)
	}
	if _, ok := root["permissions"].(map[string]any)["ask"]; ok {
		t.Error("an ask rule was added; guards never prompt")
	}
	summary, _ = applyCredentialDeny(root)
	if !strings.Contains(summary, "already present") {
		t.Errorf("second run %q", summary)
	}
	for _, bad := range []map[string]any{{"permissions": "x"}, {"permissions": map[string]any{"deny": "x"}}} {
		if _, err := applyCredentialDeny(bad); err == nil {
			t.Errorf("malformed %v accepted", bad)
		}
	}
}

func TestApplyAttribution(t *testing.T) {
	root := map[string]any{"attribution": map[string]any{"commit": "Co-Authored-By: x", "sessionUrl": false, "pr": "keep"}}
	if _, err := applyAttribution(root); err != nil {
		t.Fatal(err)
	}
	attr := root["attribution"].(map[string]any)
	if attr["commit"] != "" || attr["sessionUrl"] != false || attr["pr"] != "keep" {
		t.Errorf("attribution = %v", attr)
	}
	if s, _ := applyAttribution(root); !strings.Contains(s, "already empty") {
		t.Errorf("second run %q", s)
	}
	fresh := map[string]any{}
	if _, err := applyAttribution(fresh); err != nil || fresh["attribution"].(map[string]any)["commit"] != "" {
		t.Errorf("fresh = %v, %v", fresh, err)
	}
	if _, err := applyAttribution(map[string]any{"attribution": true}); err == nil {
		t.Error("malformed attribution accepted")
	}
}

// TestClaudeCodeSafetyNetInstall runs the whole Claude Code install against a
// temp home with a legacy plugin and a user rulebook, through the simulated
// claude CLI, and checks the migration, the preservation and the repeat run.
func TestClaudeCodeSafetyNetInstall(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	fake := fakeclaude.New(loc.ClaudeDir)
	t.Cleanup(SetSafetyNetRunner(func(safetynet.Locations) safetynet.Runner { return fake }))

	writeJSON := func(path string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(loc.ClaudeDir, "plugins", "installed_plugins.json"), map[string]any{"version": 2, "plugins": map[string]any{
		"safety-net@cc-marketplace": []any{map[string]any{"scope": "user", "version": "0.7.1"}},
	}})
	writeJSON(filepath.Join(loc.ClaudeDir, "plugins", "known_marketplaces.json"), map[string]any{
		"cc-marketplace": map[string]any{"source": map[string]any{"source": "github", "repo": "kenryu42/cc-marketplace"}},
	})
	writeJSON(filepath.Join(loc.ClaudeDir, "settings.json"), map[string]any{
		"model":       "opus",
		"permissions": map[string]any{"deny": []any{"Bash(sudo *)"}, "allow": []any{"Bash"}},
		"attribution": map[string]any{"sessionUrl": false},
	})
	userRB := filepath.Join(loc.RulesDir(), "willesq-guards", "rulebook.json")
	writeJSON(userRB, map[string]any{"rulebook_version": 1, "name": "willesq-guards", "version": "1.0.0", "rules": []any{}})
	writeJSON(loc.RuleConfigPath(), map[string]any{"version": 1, "rules": []any{"willesq-guards"}, "overrides": map[string]any{"willesq-guards/x": "off"}})
	userRBBefore, _ := os.ReadFile(userRB)

	plan := BuildPlan(ClaudeCode{}, testBins(), home)
	var dry bytes.Buffer
	if err := plan.Print(&dry); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"=== RUN ===", "claude plugin uninstall safety-net@cc-marketplace --scope user",
		"claude plugin install cc-safety-net@orch-pinned --scope user", "marketplace remove cc-marketplace", "step(s) would run"} {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("dry run lacks %q", want)
		}
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("dry run ran %v", fake.Calls())
	}

	var out bytes.Buffer
	if err := plan.Apply(&out, filepath.Join(t.TempDir(), "backups")); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	wantCalls := []string{
		"plugin marketplace add " + loc.MarketplaceDir,
		"plugin uninstall safety-net@cc-marketplace --scope user",
		"plugin install cc-safety-net@orch-pinned --scope user",
		"plugin marketplace remove cc-marketplace",
	}
	if !slices.Equal(fake.Calls(), wantCalls) {
		t.Errorf("calls %q\nwant  %q", fake.Calls(), wantCalls)
	}
	st := safetynet.Detect(loc)
	if !st.OK() {
		t.Errorf("not ok after install: %+v", st)
	}

	// settings: deny rules added and deduped, attribution set, personal keys kept.
	var settings map[string]any
	raw, _ := os.ReadFile(filepath.Join(loc.ClaudeDir, "settings.json"))
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	deny := settings["permissions"].(map[string]any)["deny"].([]any)
	if len(deny) != 4 || deny[0] != "Bash(sudo *)" {
		t.Errorf("deny = %v", deny)
	}
	if _, ok := settings["permissions"].(map[string]any)["ask"]; ok {
		t.Error("ask rules added")
	}
	if a := settings["attribution"].(map[string]any); a["commit"] != "" || a["sessionUrl"] != false {
		t.Errorf("attribution = %v", a)
	}
	if settings["model"] != "opus" {
		t.Errorf("personal key changed: %v", settings["model"])
	}

	// rules: the user rulebook is untouched and still listed first.
	if after, _ := os.ReadFile(userRB); !bytes.Equal(after, userRBBefore) {
		t.Error("user rulebook modified")
	}
	var cfg map[string]any
	raw, _ = os.ReadFile(loc.RuleConfigPath())
	_ = json.Unmarshal(raw, &cfg)
	if r := cfg["rules"].([]any); len(r) != 2 || r[0] != "willesq-guards" || r[1] != "orch-guards" {
		t.Errorf("rules = %v", r)
	}
	if cfg["overrides"].(map[string]any)["willesq-guards/x"] != "off" {
		t.Errorf("override lost: %v", cfg["overrides"])
	}
	if info, _ := os.Stat(loc.RuleConfigPath()); info.Mode().Perm() != 0o600 {
		t.Errorf("rule.json mode %v, want 0600 kept", info.Mode().Perm())
	}

	// A repeat install is a no-op: nothing written, nothing run.
	n, err := plan.Pending()
	if err != nil || n != 0 {
		t.Errorf("Pending() = %d, %v after install", n, err)
	}
	out.Reset()
	if err := plan.Apply(&out, filepath.Join(t.TempDir(), "b2")); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls()) != len(wantCalls) || !strings.Contains(out.String(), "already fully installed") {
		t.Errorf("second apply ran %v:\n%s", fake.Calls(), out.String())
	}
}

func TestSafetyNetPlanStepFailure(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	fake := fakeclaude.New(loc.ClaudeDir)
	fake.Fail = map[string]error{"plugin install": errors.New("exit status 1")}
	plan := SafetyNetPlan(loc, SafetyNetOptions{Runner: fake})
	var out bytes.Buffer
	err := plan.ApplyContext(context.Background(), &out, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "install cc-safety-net@orch-pinned") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "simulated failure") {
		t.Errorf("output lacks the command output:\n%s", out.String())
	}
	// The marketplace was added before the failure; a retry only installs.
	fake.Fail = nil
	if err := plan.ApplyContext(context.Background(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if last := calls[len(calls)-1]; last != "plugin install cc-safety-net@orch-pinned --scope user" || len(calls) != 3 {
		t.Errorf("calls %q", calls)
	}
}

func TestSafetyNetPlanRulesOnly(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	fake := fakeclaude.New(loc.ClaudeDir)
	plan := SafetyNetPlan(loc, SafetyNetOptions{RulesOnly: true, Runner: fake})
	if err := plan.Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("rules-only ran %v", fake.Calls())
	}
	if _, err := os.Stat(loc.ManifestPath()); !os.IsNotExist(err) {
		t.Error("rules-only wrote the marketplace")
	}
	if !safetynet.DetectRules(loc).OK() {
		t.Error("rules not repaired")
	}
}

func TestSafetyNetRefusesBrokenRuleConfig(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	if err := os.MkdirAll(loc.RulesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.RuleConfigPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SafetyNetPlan(loc, SafetyNetOptions{RulesOnly: true}).Apply(&bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(loc.RuleConfigPath()); string(b) != "{broken" {
		t.Error("broken rule.json rewritten")
	}
}

// noClaude is a runner on a machine without the claude CLI.
type noClaude struct{}

func (noClaude) Run(context.Context, []string) (string, error) { return "", safetynet.ErrNoClaude }

func TestSafetyNetWithoutClaude(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetSafetyNetRunner(func(safetynet.Locations) safetynet.Runner { return noClaude{} }))
	var out bytes.Buffer
	if err := BuildPlan(ClaudeCode{}, testBins(), home).Apply(&out, t.TempDir()); err != nil {
		t.Fatalf("install without claude failed: %v", err)
	}
	if !strings.Contains(out.String(), "skipped: claude CLI not found") {
		t.Errorf("no skip note:\n%s", out.String())
	}
	// orchd's action reports the missing CLI as a failure instead.
	err := SafetyNetPlan(safetynet.Locate(home), SafetyNetOptions{Runner: noClaude{}}).Apply(&bytes.Buffer{}, t.TempDir())
	if !errors.Is(err, safetynet.ErrNoClaude) {
		t.Errorf("err = %v", err)
	}
}

func TestSafetyNetVerifiesThePin(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	fake := fakeclaude.New(loc.ClaudeDir)
	fake.SHA = strings.Repeat("b", 40)
	err := SafetyNetPlan(loc, SafetyNetOptions{Runner: fake}).Apply(&bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("err = %v", err)
	}

	if _, err := verifyPin(safetynet.Locate(t.TempDir()), false); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing: %v", err)
	}
	if out, err := verifyPin(safetynet.Locate(t.TempDir()), true); err != nil || !strings.HasPrefix(out, "skipped") {
		t.Errorf("missing, skipped: %q %v", out, err)
	}
	noSHA := safetynet.Locate(t.TempDir())
	writePlugins(t, noSHA, map[string]any{"scope": "user", "version": safetynet.Pinned.Version})
	if _, err := verifyPin(noSHA, false); err == nil || !strings.Contains(err.Error(), "no commit") {
		t.Errorf("no sha: %v", err)
	}
	good := safetynet.Locate(t.TempDir())
	writePlugins(t, good, map[string]any{"scope": "user", "version": safetynet.Pinned.Version, "gitCommitSha": safetynet.Pinned.SHA})
	if out, err := verifyPin(good, false); err != nil || !strings.HasPrefix(out, "verified") {
		t.Errorf("good: %q %v", out, err)
	}
}

func writePlugins(t *testing.T, loc safetynet.Locations, entry map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{safetynet.PluginID: []any{entry}}})
	path := filepath.Join(loc.ClaudeDir, "plugins", "installed_plugins.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
