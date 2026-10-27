package safetynet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// PluginState summarizes how cc-safety-net is installed in Claude Code.
type PluginState string

const (
	// StateMissing: no cc-safety-net install at all.
	StateMissing PluginState = "missing"
	// StateLegacy: a copy from another marketplace (e.g. the old
	// safety-net@cc-marketplace) is installed, with or without ours.
	StateLegacy PluginState = "legacy"
	// StateOutdated: ours is installed at a version or commit other than the pin.
	StateOutdated PluginState = "outdated"
	// StateDisabled: ours is installed at the pin but disabled in settings.
	StateDisabled PluginState = "disabled"
	// StateOK: ours is installed at the pin and enabled.
	StateOK PluginState = "ok"
)

// Install is one plugin install record from installed_plugins.json.
type Install struct {
	ID          string
	Name        string
	Marketplace string
	Scope       string
	Version     string
	SHA         string
	Path        string
}

// State is the full safety-net picture for one home.
type State struct {
	Plugin PluginState
	Pinned Spec
	// Installed is our user-scope install, when present.
	Installed *Install
	// Legacy are competing installs of the plugin from other marketplaces.
	Legacy []Install
	// MarketplaceKnown is true when Claude Code knows orch-pinned;
	// MarketplacePath is where it points and MarketplaceMismatch is true when
	// that is not the directory orch writes.
	MarketplaceKnown    bool
	MarketplacePath     string
	MarketplaceMismatch bool
	// LegacyMarketplaceKnown is true when cc-marketplace is registered, and
	// LegacyMarketplaceShared when other installed plugins still come from it.
	LegacyMarketplaceKnown  bool
	LegacyMarketplaceShared bool
	Rules                   RulesState
	// Errors name plugin files that exist but could not be read.
	Errors []string
}

// OK reports whether the plugin and the guard rules are both as orch expects.
func (s State) OK() bool { return s.Plugin == StateOK && s.Rules.OK() }

// Detect reads Claude Code's plugin files and the cc-safety-net rules.
func Detect(loc Locations) State {
	st := State{Pinned: Pinned, Rules: DetectRules(loc)}
	installs, err := readInstalls(filepath.Join(loc.pluginsDir(), "installed_plugins.json"))
	if err != nil {
		st.Errors = append(st.Errors, "installed_plugins.json: "+err.Error())
	}
	markets, err := readMarketplaces(filepath.Join(loc.pluginsDir(), "known_marketplaces.json"))
	if err != nil {
		st.Errors = append(st.Errors, "known_marketplaces.json: "+err.Error())
	}
	enabled := readEnabled(loc.settingsPath())

	for _, in := range installs {
		switch {
		case in.ID == PluginID:
			// Reason: only user scope counts as "installed by orch". A
			// project-scope copy only covers one repo and is the user's call.
			if in.Scope == "user" && st.Installed == nil {
				cp := in
				st.Installed = &cp
			}
		case slices.Contains(legacyPluginNames, in.Name):
			st.Legacy = append(st.Legacy, in)
		case in.Marketplace == LegacyMarketplace:
			st.LegacyMarketplaceShared = true
		}
	}
	if m, ok := markets[MarketplaceName]; ok {
		st.MarketplaceKnown = true
		st.MarketplacePath = m
		st.MarketplaceMismatch = filepath.Clean(m) != filepath.Clean(loc.MarketplaceDir)
	}
	_, st.LegacyMarketplaceKnown = markets[LegacyMarketplace]

	switch {
	case len(st.Legacy) > 0:
		st.Plugin = StateLegacy
	case st.Installed == nil:
		st.Plugin = StateMissing
	case st.Installed.Version != Pinned.Version ||
		(st.Installed.SHA != "" && st.Installed.SHA != Pinned.SHA) ||
		st.MarketplaceMismatch:
		st.Plugin = StateOutdated
	case enabled[PluginID] != nil && !*enabled[PluginID]:
		st.Plugin = StateDisabled
	default:
		st.Plugin = StateOK
	}
	return st
}

// readJSONFile reads a small JSON file; a missing file is not an error.
func readJSONFile(path string, v any) (bool, error) {
	raw, err := readSmall(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// readInstalls flattens installed_plugins.json into one record per scope,
// sorted by ID then scope so results are stable.
func readInstalls(path string) ([]Install, error) {
	var doc struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
			SHA         string `json:"gitCommitSha"`
		} `json:"plugins"`
	}
	if _, err := readJSONFile(path, &doc); err != nil {
		return nil, err
	}
	var out []Install
	for id, entries := range doc.Plugins {
		name, market, _ := strings.Cut(id, "@")
		for _, e := range entries {
			out = append(out, Install{
				ID: id, Name: name, Marketplace: market, Scope: e.Scope,
				Version: e.Version, SHA: e.SHA, Path: e.InstallPath,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// readMarketplaces maps each known marketplace to its install location (for a
// directory source, the directory itself).
func readMarketplaces(path string) (map[string]string, error) {
	doc := map[string]struct {
		Source struct {
			Source string `json:"source"`
			Path   string `json:"path"`
		} `json:"source"`
		InstallLocation string `json:"installLocation"`
	}{}
	if _, err := readJSONFile(path, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(doc))
	for name, m := range doc {
		loc := m.InstallLocation
		if m.Source.Source == "directory" && m.Source.Path != "" {
			loc = m.Source.Path
		}
		out[name] = loc
	}
	return out, nil
}

// readEnabled reads settings.json enabledPlugins; an unreadable file reads as
// no explicit setting, which Claude Code treats as enabled.
func readEnabled(path string) map[string]*bool {
	var doc struct {
		Enabled map[string]any `json:"enabledPlugins"`
	}
	if ok, err := readJSONFile(path, &doc); !ok || err != nil {
		return nil
	}
	out := map[string]*bool{}
	for k, v := range doc.Enabled {
		if b, ok := v.(bool); ok {
			out[k] = &b
		}
	}
	return out
}
