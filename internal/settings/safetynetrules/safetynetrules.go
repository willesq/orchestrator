// Package safetynetrules is the settings provider for cc-safety-net's custom
// rules: each rule of each rulebook listed in the user-scope rule.json is one
// item, turned off with cc-safety-net's documented override,
// "overrides": {"<rulebook>/<rule>": "off"}. cc-safety-net reads rule.json on
// every command, so a change applies to the next one.
//
// Rules are toggled only. Rulebooks are their authors' files (orch-guards is
// rewritten by orch install), so rules are neither added nor removed here.
package safetynetrules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
)

// KindRule is the one kind this provider manages.
const KindRule = "rule"

// Provider manages the rules of one cc-safety-net home.
type Provider struct {
	loc safetynet.Locations
}

// New returns the provider for cc-safety-net's locations.
func New(loc safetynet.Locations) *Provider { return &Provider{loc: loc} }

// Name implements settings.Provider.
func (*Provider) Name() string { return install.SafetyNetRulesProvider }

// Title implements settings.Provider.
func (*Provider) Title() string { return "cc-safety-net" }

// Kinds implements settings.Provider.
func (*Provider) Kinds() []settings.KindSpec {
	return []settings.KindSpec{{Name: KindRule, Title: "Safety Net rules", GroupTitle: "Rulebook", StateInFile: true}}
}

// rulePayload is what a rule is listed with.
type rulePayload struct {
	Rulebook string   `json:"rulebook"`
	Rule     string   `json:"rule"`
	Source   string   `json:"source"`
	Command  string   `json:"command,omitempty"`
	Blocks   []string `json:"block_args,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// ruleConfig is the part of rule.json the provider reads.
type ruleConfig struct {
	Rules     []string       `json:"rules"`
	Overrides map[string]any `json:"overrides"`
}

// Read implements settings.Provider: rule.json and every local rulebook file
// it lists.
func (p *Provider) Read() (map[string][]byte, error) {
	cfgPath := p.loc.RuleConfigPath()
	raw, err := settings.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{cfgPath: raw}
	var cfg ruleConfig
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &cfg) != nil {
		// Observe reports the parse error.
		return files, nil
	}
	for _, src := range cfg.Rules {
		name, _ := safetynet.SourceName(src)
		path := p.loc.RulebookPath(name)
		if path == "" {
			continue
		}
		b, err := settings.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files[path] = b
	}
	return files, nil
}

// Observe implements settings.Provider.
func (p *Provider) Observe(files map[string][]byte) ([]settings.Item, error) {
	raw := files[p.loc.RuleConfigPath()]
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var cfg ruleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse rule.json: %w", err)
	}
	var out []settings.Item
	seen := map[string]bool{}
	for _, src := range cfg.Rules {
		name, remote := safetynet.SourceName(src)
		path := p.loc.RulebookPath(name)
		if path == "" || files[path] == nil {
			// Missing or invalid: cc-safety-net skips it, so it has no rules.
			continue
		}
		rb := safetynet.ParseRulebook(safetynet.RulebookInfo{Source: src, Name: name, Path: path},
			files[path], cfg.Overrides)
		if rb.Error != "" {
			continue
		}
		for _, r := range rb.Rules {
			id := name + "/" + r.Name
			if seen[id] || !safetynet.ValidName(r.Name) {
				continue
			}
			seen[id] = true
			origin := settings.OriginFound
			if name == safetynet.RulebookName && !remote {
				origin = settings.OriginOrch
			}
			cmd := strings.TrimSpace(r.Command + " " + r.Subcommand)
			out = append(out, settings.Item{
				Kind: KindRule, Identity: id, Enabled: !r.Off, Origin: origin,
				Payload: mustJSON(rulePayload{Rulebook: name, Rule: r.Name, Source: src, Command: cmd,
					Blocks: r.BlockArgs, Reason: r.Reason}),
			})
		}
	}
	return out, nil
}

// Apply implements settings.Provider: it sets or clears the rule's "off"
// override, preserving every other key of rule.json.
func (p *Provider) Apply(files map[string][]byte, it settings.Item, enabled bool) (map[string][]byte, error) {
	if it.Kind != KindRule {
		return nil, settings.Invalidf("cc-safety-net has no kind %q", it.Kind)
	}
	book, rule, ok := strings.Cut(it.Identity, "/")
	if !ok || !safetynet.ValidName(book) || !safetynet.ValidName(rule) {
		return nil, settings.Invalidf("%q is not a <rulebook>/<rule> name", it.Identity)
	}
	path := p.loc.RuleConfigPath()
	root := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(files[path]))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse rule.json: %w — refusing to rewrite it", err)
	}
	if v, ok := root["version"].(json.Number); !ok || v.String() != "1" {
		return nil, fmt.Errorf("%w: rule.json version %v is not 1 — refusing to rewrite it", settings.ErrInvalid, root["version"])
	}
	var overrides map[string]any
	switch v := root["overrides"].(type) {
	case nil:
		overrides = map[string]any{}
	case map[string]any:
		overrides = v
	default:
		return nil, fmt.Errorf("%w: rule.json: overrides is %T, not an object — refusing to rewrite it", settings.ErrInvalid, v)
	}
	cur, _ := overrides[it.Identity].(string)
	switch {
	case !enabled && cur == "off", enabled && cur != "off":
		return nil, nil
	case enabled:
		delete(overrides, it.Identity)
	default:
		overrides[it.Identity] = "off"
	}
	root["overrides"] = overrides
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return map[string][]byte{path: buf.Bytes()}, nil
}

// Validate implements settings.Provider; rules are not added here.
func (*Provider) Validate(kind string, _ map[string]string) (settings.Item, error) {
	return settings.Item{}, settings.Invalidf("%s items cannot be added here; edit your rulebook", kind)
}

// Describe implements settings.Provider.
func (*Provider) Describe(_ string, payload json.RawMessage) (string, string) {
	var r rulePayload
	if json.Unmarshal(payload, &r) != nil {
		return "", ""
	}
	summary := r.Rule
	if r.Command != "" {
		summary += ": " + r.Command
		if len(r.Blocks) > 0 {
			summary += " " + strings.Join(r.Blocks, " ")
		}
	}
	return r.Rulebook, summary
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
