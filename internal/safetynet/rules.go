package safetynet

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// rulebookJSON is the orch-guards rulebook: the shared-policy guards only.
// Personal rules belong in a user-owned rulebook, which orch never touches.
//
//go:embed orch-guards.rulebook.json
var rulebookJSON []byte

// Rulebook returns the embedded orch-guards rulebook bytes.
func Rulebook() []byte { return bytes.Clone(rulebookJSON) }

// nameRe is cc-safety-net's rulebook and rule name pattern. Requiring it of
// every source name before it becomes a path segment also rules out traversal.
var nameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

func validName(s string) bool { return nameRe.MatchString(s) }

// ValidName reports whether s is a valid cc-safety-net rulebook or rule
// name. Every name orch turns into a path or an override key is checked.
func ValidName(s string) bool { return validName(s) }

// maxRuleFile bounds how much of a rule file is read. Real rulebooks are a few
// KiB; the cap keeps a stray huge file from stalling a status call.
const maxRuleFile = 1 << 20

// readSmall reads a regular file of at most maxRuleFile bytes. Anything else
// (a FIFO would block the open) is refused before it is opened.
func readSmall(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRuleFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxRuleFile {
		return nil, fmt.Errorf("larger than %d bytes", maxRuleFile)
	}
	return b, nil
}

// marshalJSON writes JSON the way orch's install does: 2-space indent and a
// trailing newline, so drift checks compare like with like.
func marshalJSON(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// NormalizeJSON re-encodes a JSON object through marshalJSON; empty stays empty.
func NormalizeJSON(old []byte) ([]byte, error) {
	if len(old) == 0 {
		return nil, nil
	}
	root := map[string]any{}
	if err := json.Unmarshal(old, &root); err != nil {
		return nil, err
	}
	return marshalJSON(root)
}

// MergeRuleConfig adds orch-guards and the rtk wrapper to a user rule.json,
// preserving every other source, override, wrapper and key ($schema included).
// An unparseable file, or one of a schema version orch does not know, is an
// error rather than a rewrite: it is the user's file.
func MergeRuleConfig(old []byte) ([]byte, string, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(old)) > 0 {
		if err := json.Unmarshal(old, &root); err != nil {
			return nil, "", fmt.Errorf("parse rule.json: %w — refusing to rewrite it; fix it and re-run", err)
		}
	}
	var notes []string
	switch v := root["version"].(type) {
	case nil:
		root["version"] = 1
		notes = append(notes, "set version 1")
	case float64:
		if v != 1 {
			return nil, "", fmt.Errorf("rule.json: version %v is not 1 — refusing to rewrite it", v)
		}
	default:
		return nil, "", fmt.Errorf("rule.json: version is %T, not a number — refusing to rewrite it", v)
	}
	for _, f := range []struct{ key, want, what string }{
		{"rules", RulebookName, "rulebook source"},
		{"transparent_wrappers", RTKWrapper, "transparent wrapper"},
	} {
		list, err := stringList(root, f.key)
		if err != nil {
			return nil, "", err
		}
		if slices.Contains(list, f.want) {
			continue
		}
		arr, _ := root[f.key].([]any)
		root[f.key] = append(arr, f.want)
		notes = append(notes, fmt.Sprintf("added %s %q", f.what, f.want))
	}
	out, err := marshalJSON(root)
	if err != nil {
		return nil, "", err
	}
	summary := "already lists " + RulebookName + " and the " + RTKWrapper + " wrapper"
	if len(notes) > 0 {
		summary = strings.Join(notes, "; ") + "; preserved every other source, override and wrapper"
	}
	return out, "rule.json: " + summary, nil
}

// stringList reads an optional array of strings, refusing any other shape.
func stringList(root map[string]any, key string) ([]string, error) {
	switch v := root[key].(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("rule.json: %s holds a %T, not a string — refusing to rewrite it", key, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("rule.json: %s is %T, not an array — refusing to rewrite it", key, v)
	}
}

// RulesState is what the user-scope cc-safety-net rules look like.
type RulesState struct {
	ConfigPath   string
	ConfigExists bool
	// ConfigError is set when rule.json cannot be read or parsed, which makes
	// cc-safety-net ignore every user-scope source.
	ConfigError string
	// Referenced is true when rule.json lists orch-guards.
	Referenced bool
	// RTKWrapper is true when rtk is a transparent wrapper.
	RTKWrapper bool
	// RulebookCurrent is true when the orch-guards file matches this build.
	RulebookCurrent bool
	// Rulebooks are the sources rule.json lists, in order.
	Rulebooks []RulebookInfo
}

// OK reports whether the guards are installed and active as orch expects.
func (r RulesState) OK() bool {
	return r.ConfigError == "" && r.Referenced && r.RTKWrapper && r.RulebookCurrent
}

// Problems lists what Repair would fix, for status lines.
func (r RulesState) Problems() []string {
	var p []string
	if r.ConfigError != "" {
		p = append(p, "rule.json unreadable: "+r.ConfigError)
	}
	if !r.Referenced {
		p = append(p, RulebookName+" not enabled in rule.json")
	}
	if !r.RTKWrapper {
		p = append(p, "rtk is not a transparent wrapper")
	}
	if !r.RulebookCurrent {
		p = append(p, RulebookName+" rulebook missing or out of date")
	}
	return p
}

// RulebookInfo describes one rulebook source listed in rule.json.
type RulebookInfo struct {
	Source string
	Name   string
	Path   string
	// Owner is "orch" for orch-guards and "user" for everything else.
	Owner       string
	Remote      bool
	Version     string
	Description string
	// Error is set when the rulebook is missing or invalid, which makes
	// cc-safety-net skip the whole source silently.
	Error string
	Rules []RuleInfo
}

// RuleInfo is one rule of a rulebook.
type RuleInfo struct {
	Name       string
	Command    string
	Subcommand string
	BlockArgs  []string
	Reason     string
	// Off is true when rule.json overrides the rule to "off".
	Off bool
}

// DetectRules reads rule.json and every rulebook it lists.
func DetectRules(loc Locations) RulesState {
	st := RulesState{ConfigPath: loc.RuleConfigPath()}
	if cur, err := readSmall(loc.RulebookPath(RulebookName)); err == nil {
		st.RulebookCurrent = sameJSON(cur, rulebookJSON)
	}
	raw, err := readSmall(st.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return st
	}
	st.ConfigExists = true
	if err != nil {
		st.ConfigError = err.Error()
		return st
	}
	var cfg struct {
		Version   any            `json:"version"`
		Rules     []string       `json:"rules"`
		Overrides map[string]any `json:"overrides"`
		Wrappers  []string       `json:"transparent_wrappers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		st.ConfigError = err.Error()
		return st
	}
	if v, ok := cfg.Version.(float64); !ok || v != 1 {
		st.ConfigError = fmt.Sprintf("version %v is not 1", cfg.Version)
	}
	st.RTKWrapper = slices.Contains(cfg.Wrappers, RTKWrapper)
	for _, src := range cfg.Rules {
		info := readRulebook(loc, src, cfg.Overrides)
		// Only the local source is orch's; a GitHub source that happens to
		// be named orch-guards is someone else's rulebook.
		if src == RulebookName {
			st.Referenced = true
		}
		st.Rulebooks = append(st.Rulebooks, info)
	}
	return st
}

// SourceName returns the rulebook name of a rule.json source: a bare local
// name, or the last segment of a GitHub source "owner/repo#ref/name".
func SourceName(src string) (name string, remote bool) { return sourceName(src) }

func sourceName(src string) (name string, remote bool) {
	if i := strings.LastIndex(src, "/"); i >= 0 {
		return src[i+1:], true
	}
	return src, false
}

// readRulebook loads one listed source for display.
func readRulebook(loc Locations, src string, overrides map[string]any) RulebookInfo {
	name, remote := sourceName(src)
	info := RulebookInfo{Source: src, Name: name, Remote: remote, Owner: "user"}
	if name == RulebookName && !remote {
		info.Owner = "orch"
	}
	info.Path = loc.RulebookPath(name)
	if info.Path == "" {
		info.Error = "invalid rulebook name"
		return info
	}
	raw, err := readSmall(info.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			info.Error = "rulebook file missing"
		} else {
			info.Error = err.Error()
		}
		return info
	}
	return ParseRulebook(info, raw, overrides)
}

// ParseRulebook fills info (Source, Name and Path already set) from a
// rulebook's bytes, marking each rule off that overrides turns off.
func ParseRulebook(info RulebookInfo, raw []byte, overrides map[string]any) RulebookInfo {
	name := info.Name
	var rb struct {
		RulebookVersion int    `json:"rulebook_version"`
		Name            string `json:"name"`
		Version         string `json:"version"`
		Description     string `json:"description"`
		Rules           []struct {
			Name       string   `json:"name"`
			Command    string   `json:"command"`
			Subcommand string   `json:"subcommand"`
			BlockArgs  []string `json:"block_args"`
			Reason     string   `json:"reason"`
			Match      struct {
				CommandPath []string `json:"command_path"`
				AnyArgs     []string `json:"any_args"`
			} `json:"match"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &rb); err != nil {
		info.Error = "invalid JSON: " + err.Error()
		return info
	}
	info.Version, info.Description = rb.Version, rb.Description
	switch {
	case rb.RulebookVersion != 1 && rb.RulebookVersion != 2:
		info.Error = fmt.Sprintf("rulebook_version %d is not 1 or 2", rb.RulebookVersion)
	case rb.Name != name:
		info.Error = fmt.Sprintf("rulebook name %q does not match source %q", rb.Name, name)
	}
	for _, r := range rb.Rules {
		ri := RuleInfo{Name: r.Name, Command: r.Command, Subcommand: r.Subcommand, BlockArgs: r.BlockArgs, Reason: r.Reason}
		if rb.RulebookVersion == 2 {
			ri.Subcommand = strings.Join(r.Match.CommandPath, " ")
			ri.BlockArgs = r.Match.AnyArgs
		}
		if v, ok := overrides[name+"/"+r.Name].(string); ok && v == "off" {
			ri.Off = true
		}
		info.Rules = append(info.Rules, ri)
	}
	return info
}

// sameJSON compares two JSON documents by meaning.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
