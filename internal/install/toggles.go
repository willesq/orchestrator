package install

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The settings provider and kinds for the items the Claude Code install
// writes into settings.json. The claude-code settings provider stores items
// under these names, so a toggle made in the app is the toggle install reads.
const (
	ProviderClaudeCode = "claude-code"
	KindHook           = "hook"
	KindPermission     = "permission"
	KindPlugin         = "plugin"
)

// HookIdentity is the settings identity of orch-hook's handler for an event.
// It names the event and the orch-hook subcommand rather than the command
// line, so a toggle survives a new binary path or new timeouts.
func HookIdentity(event, slug string) string { return event + "/orch-hook " + slug }

// PermissionIdentity is the settings identity of a permission rule.
func PermissionIdentity(list, rule string) string { return list + "/" + rule }

// plainBinRe is a binary path install could have written: no spaces, quotes
// or shell metacharacters.
var plainBinRe = regexp.MustCompile(`^[A-Za-z0-9_./~+@-]+$`)

// OrchHookSlugFor returns the orch-hook subcommand of a Claude Code hook
// command when the command is exactly what install writes for that event:
// "<dir>/orch-hook <slug>", with the slug install maps to the event.
//
// Reason: exact, not a substring match. A command that merely mentions
// orch-hook ("curl … | sh; orch-hook session-start") is someone else's hook;
// treating it as orch's would let it take over orch-hook's identity, and
// with it a toggle the user made, and hide it from Remove.
func OrchHookSlugFor(event, command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) != 2 || filepath.Base(fields[0]) != "orch-hook" || !plainBinRe.MatchString(fields[0]) ||
		strings.Join(fields, " ") != command {
		return "", false
	}
	for _, ev := range eventSlugs {
		if ev.Event == event && ev.Slug == fields[1] {
			return ev.Slug, true
		}
	}
	return "", false
}

// OrchRule is a permission rule the Claude Code install writes.
type OrchRule struct{ List, Rule string }

// OrchPermissionRules lists every permission rule the Claude Code install
// may write: the allow rules for orch's MCP servers and the credential-file
// Read denies.
func OrchPermissionRules() []OrchRule {
	var out []OrchRule
	for _, s := range orchMCPServers {
		out = append(out, OrchRule{"allow", mcpToolRule(s)})
	}
	for _, r := range credentialReadDeny {
		out = append(out, OrchRule{"deny", r})
	}
	return out
}

// loadToggles reads what the user turned off in orch's settings store.
var loadToggles = func() (settings.Toggles, error) { return settings.LoadToggles(paths.DBFile()) }

// loadSwitches reads the enforcement switches from orch.db; install reads
// the hooks switch (D-40).
var loadSwitches = func() (spaces.Enforcement, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return spaces.ReadEnforcement(ctx, paths.DBFile())
}

// SetSwitchLoader replaces how install reads the enforcement switches and
// returns a function restoring the previous loader, for tests in other
// packages.
func SetSwitchLoader(f func() (spaces.Enforcement, error)) (restore func()) {
	prev := loadSwitches
	loadSwitches = f
	return func() { loadSwitches = prev }
}

// toolHooksOff reports whether the operator switched the tool hooks off,
// counting a timed switch whose time is up as reverted (orchd may not be
// running to revert it). Reason, fail closed: switches that cannot be read,
// or that do not match their change log, leave the hooks on, so install
// writes them; they are off only by the operator's switch.
func toolHooksOff() bool {
	sw, err := loadSwitches()
	return err == nil && sw.At(time.Now()).State(spaces.LayerHooks) == spaces.SwitchOff
}

// isToolHookEvent reports whether event and slug are one of the tool hooks
// spaces depend on, which only the hooks switch turns off.
func isToolHookEvent(event, slug string) bool {
	for _, ev := range toolHookEvents[ProviderClaudeCode].events {
		if ev.Event == event && ev.Slug == slug {
			return true
		}
	}
	return false
}

// SetToggleLoader replaces how install reads the settings toggles and
// returns a function restoring the previous loader. It exists for tests in
// other packages, which must never read the developer's own orch.db.
func SetToggleLoader(f func() (settings.Toggles, error)) (restore func()) {
	prev := loadToggles
	loadToggles = f
	return func() { loadToggles = prev }
}

// DisabledCount is how many Claude Code items (settings.json and
// cc-safety-net rules) and instruction modules the user turned off in orch,
// for orch status.
func DisabledCount() (int, error) {
	t, err := loadToggles()
	if err != nil {
		return 0, err
	}
	return t.Count(ProviderClaudeCode) + t.Count(SafetyNetRulesProvider) + t.Count(InstructionsProvider), nil
}

// SensitiveModulesOff names the sensitive instruction modules (the agents'
// security policy) and security review agents turned off on this machine,
// for orch status.
func SensitiveModulesOff() ([]string, error) {
	t, err := loadToggles()
	if err != nil {
		return nil, err
	}
	mods, err := Modules()
	if err != nil {
		return nil, err
	}
	on := ModuleOn(t)
	var off []string
	for _, m := range mods {
		if m.Sensitive && !on(m) {
			off = append(off, m.Slug)
		}
	}
	assets, err := ClaudeAssets()
	if err != nil {
		return nil, err
	}
	assetOn := AssetEnabled(t)
	for _, a := range assets {
		if a.Sensitive && !assetOn(a) {
			off = append(off, a.Kind+" "+a.Name)
		}
	}
	return off, nil
}

// SafetyNetRulesProvider is the settings provider for cc-safety-net rules.
const SafetyNetRulesProvider = "cc-safety-net"

// BackupFunc returns a function that backs a file's bytes up under
// backupDir, in a timestamped directory, the way Apply does.
func BackupFunc(backupDir string) func(path string, content []byte) error {
	return func(path string, content []byte) error {
		stamp := time.Now().UTC().Format("20060102-150405")
		return backup(filepath.Join(backupDir, stamp), path, content)
	}
}

// ModuleOn reads a module's state from the toggles: the choice stored for
// this machine, or the module's default when it was never toggled.
func ModuleOn(t settings.Toggles) ModuleEnabled {
	return func(m Module) bool {
		if on, known := t.State(InstructionsProvider, KindModule, m.Slug); known {
			return on
		}
		return m.Default
	}
}
