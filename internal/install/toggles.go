package install

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/settings"
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

// SetToggleLoader replaces how install reads the settings toggles and
// returns a function restoring the previous loader. It exists for tests in
// other packages, which must never read the developer's own orch.db.
func SetToggleLoader(f func() (settings.Toggles, error)) (restore func()) {
	prev := loadToggles
	loadToggles = f
	return func() { loadToggles = prev }
}

// DisabledCount is how many Claude Code items (settings.json and
// cc-safety-net rules) the user turned off in orch, for orch status.
func DisabledCount() (int, error) {
	t, err := loadToggles()
	if err != nil {
		return 0, err
	}
	return t.Count(ProviderClaudeCode) + t.Count(SafetyNetRulesProvider), nil
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
