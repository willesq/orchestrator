package safetynet

import (
	"os"
	"path/filepath"

	"github.com/willesq/orchestrator/internal/paths"
)

// Locations are the directories safety-net detection and installation use.
type Locations struct {
	// Home is the user home the install targets.
	Home string
	// ClaudeDir is Claude Code's config directory (<home>/.claude).
	ClaudeDir string
	// CCSNHome is cc-safety-net's user root (CC_SAFETY_NET_HOME or
	// <home>/.cc-safety-net); rules live under <CCSNHome>/rules.
	CCSNHome string
	// MarketplaceDir is the orch-owned local marketplace directory.
	MarketplaceDir string
	// Relocated is true when Home is not the real user home, as with
	// `orch install --home`. Commands then run with HOME pointed at it.
	Relocated bool
}

// Locate resolves the locations for an install into home.
//
// Reason: CC_SAFETY_NET_HOME and the orch data directory are honored only for
// the real home. A relocated home (`orch install --home <tmp>`) is a test or
// preview, and following either variable there would write the real rules or
// register a marketplace in the real data directory.
func Locate(home string) Locations {
	real, _ := os.UserHomeDir()
	relocated := !SameDir(home, real)
	loc := Locations{
		Home:      home,
		ClaudeDir: filepath.Join(home, ".claude"),
		CCSNHome:  filepath.Join(home, ".cc-safety-net"),
		Relocated: relocated,
	}
	dataDir := filepath.Join(home, ".local", "share", "orch")
	if !relocated {
		if d := os.Getenv("CC_SAFETY_NET_HOME"); d != "" && filepath.IsAbs(d) {
			loc.CCSNHome = filepath.Clean(d)
		}
		dataDir = paths.DataDir()
	}
	loc.MarketplaceDir = filepath.Join(dataDir, "marketplaces", MarketplaceName)
	return loc
}

// RulesDir is cc-safety-net's user-scope rules directory.
func (l Locations) RulesDir() string { return filepath.Join(l.CCSNHome, "rules") }

// RuleConfigPath is the user-scope rule.json.
func (l Locations) RuleConfigPath() string { return filepath.Join(l.RulesDir(), "rule.json") }

// RulebookPath is the rulebook file for a local source name, or "" when the
// name is not a valid rulebook name (which also rules out path traversal).
func (l Locations) RulebookPath(name string) string {
	if !validName(name) {
		return ""
	}
	return filepath.Join(l.RulesDir(), name, "rulebook.json")
}

// ManifestPath is the orch marketplace's manifest.
func (l Locations) ManifestPath() string {
	return filepath.Join(l.MarketplaceDir, ".claude-plugin", "marketplace.json")
}

// pluginsDir is Claude Code's plugin state directory.
func (l Locations) pluginsDir() string { return filepath.Join(l.ClaudeDir, "plugins") }

// settingsPath is Claude Code's user settings file.
func (l Locations) settingsPath() string { return filepath.Join(l.ClaudeDir, "settings.json") }

// SameDir reports whether a and b name the same directory: equal once
// cleaned, or the same file once links are followed. Reason: a --home that
// is the real home under another spelling (a symlink to it, another case on
// a case-insensitive volume) must count as the real home, or a "relocated"
// run would skip launchd and the real data directory while still editing
// the real home's files.
func SameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
