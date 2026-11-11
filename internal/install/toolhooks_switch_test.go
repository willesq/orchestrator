package install

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestToolHooksFollowOnlyTheSwitch is the install half of SP-070.1.a4: a
// settings toggle cannot keep the tool hooks out of Claude Code's settings;
// only the operator's hooks switch does, and switches that cannot be read
// leave them in.
func TestToolHooksFollowOnlyTheSwitch(t *testing.T) {
	bins := Bins{Orch: "/o/orch", OrchHook: "/o/orch-hook", Orchd: "/o/orchd"}
	defer SetToggleLoader(func() (settings.Toggles, error) {
		return togglesOf(
			off(ProviderClaudeCode, KindHook, HookIdentity("PreToolUse", "pre-tool-use")),
			off(ProviderClaudeCode, KindHook, HookIdentity("CwdChanged", "cwd-changed")),
			off(ProviderClaudeCode, KindHook, HookIdentity("Stop", "stop")),
		), nil
	})()
	hooksOff := spaces.DefaultEnforcement()
	hooksOff[spaces.LayerHooks] = spaces.Switch{Layer: spaces.LayerHooks, State: spaces.SwitchOff}
	expired := spaces.DefaultEnforcement()
	expired[spaces.LayerHooks] = spaces.Switch{Layer: spaces.LayerHooks, State: spaces.SwitchOff,
		Until: time.Now().Add(-time.Minute), Previous: spaces.SwitchOn}
	tests := []struct {
		name    string
		load    func() (spaces.Enforcement, error)
		written bool
	}{
		{"switch on, toggles off", func() (spaces.Enforcement, error) { return spaces.DefaultEnforcement(), nil }, true},
		{"switches unreadable", func() (spaces.Enforcement, error) { return nil, errors.New("database is locked") }, true},
		{"switch off", func() (spaces.Enforcement, error) { return hooksOff, nil }, false},
		{"timed switch off, expired", func() (spaces.Enforcement, error) { return expired, nil }, true},
		{"switch off but not matching its log", func() (spaces.Enforcement, error) { return hooksOff, errors.New("does not match") }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer SetSwitchLoader(tt.load)()
			out, summary, err := ClaudeCode{}.settingsEdit(bins, t.TempDir()).Compute(nil)
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			for _, slug := range []string{"orch-hook pre-tool-use", "orch-hook cwd-changed"} {
				if strings.Contains(s, slug) != tt.written {
					t.Errorf("%s written = %v, want %v", slug, !tt.written, tt.written)
				}
			}
			if strings.Contains(s, "orch-hook stop") || !strings.Contains(summary, "left Stop") {
				t.Errorf("an ordinary toggled-off hook was written:\n%s", summary)
			}
		})
	}
}
