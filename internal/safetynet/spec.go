// Package safetynet installs and checks cc-safety-net, the Claude Code plugin
// that blocks destructive shell commands, pinned to a reviewed release, plus the
// orch-owned guard rulebook that enforces the shared agent policy on top of it.
//
// The package is pure detection and planning over files Claude Code and
// cc-safety-net own (none of which hold secrets) plus a small runner for the
// `claude plugin` commands that change them. `orch install`, orchd and Orch.app
// all read the same State, so they never disagree about what is installed.
package safetynet

// Spec pins one release of cc-safety-net.
//
// Policy: pin a release that is at least 14 days old, after reading the
// upstream diff since the previous pin. Bumping is a deliberate code change in
// a new orch release, never something a running install decides on its own.
type Spec struct {
	// Version is the plugin version Claude Code records after install.
	Version string
	// RepoURL, Ref and SHA locate the plugin tree the marketplace serves.
	RepoURL string
	Ref     string
	SHA     string
	// ReleasedOn is the upstream release date (UTC, YYYY-MM-DD), kept so the
	// 14-day policy can be checked in review and in tests.
	ReleasedOn string
	// CLIVersion is the npm release of the same code, used only by the opt-in
	// test that runs the pinned analyzer; orch never runs npx at runtime.
	CLIVersion string
}

// Pinned is the cc-safety-net release this build of orch installs.
var Pinned = Spec{
	Version:    "2.6.10",
	RepoURL:    "https://github.com/kenryu42/cc-safety-net.git",
	Ref:        "plugin",
	SHA:        "c745205d0ed4684cea070e382177d48c21b11154",
	ReleasedOn: "2026-10-12",
	CLIVersion: "2.6.10",
}

const (
	// PluginName is the plugin's name in its marketplace.
	PluginName = "cc-safety-net"
	// MarketplaceName is the orch-owned local marketplace serving the pin.
	MarketplaceName = "orch-pinned"
	// PluginID is the install identity Claude Code records.
	PluginID = PluginName + "@" + MarketplaceName
	// LegacyMarketplace is the upstream marketplace the plugin used to ship
	// from, under the old name "safety-net".
	LegacyMarketplace = "cc-marketplace"
	// RulebookName is the orch-owned guard rulebook.
	RulebookName = "orch-guards"
	// RTKWrapper must be a transparent wrapper, or a command rewritten to
	// `rtk git push ...` hides the real command from every rule.
	RTKWrapper = "rtk"
)

// legacyPluginNames are the names cc-safety-net has shipped under. An install
// of either from any marketplace other than ours is a competing copy whose
// hook would run twice.
var legacyPluginNames = []string{"safety-net", PluginName}
