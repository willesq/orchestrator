// Package claudecode is the settings provider for Claude Code's user
// settings file, ~/.claude/settings.json: each hook handler, each permission
// rule and each enabledPlugins entry is one item.
//
// Turning a hook or a permission rule off removes it from the file (the
// store keeps its payload); turning a plugin off sets it to false, as
// `claude plugin disable` does. Claude Code reloads the file when it changes.
package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/settings"
)

// Provider manages one settings.json.
type Provider struct {
	path string
}

// New returns the provider for the Claude Code settings under home.
func New(home string) *Provider {
	return &Provider{path: filepath.Join(home, ".claude", "settings.json")}
}

// Path is the settings file the provider manages.
func (p *Provider) Path() string { return p.path }

// Name implements settings.Provider; it matches install's adapter name.
func (*Provider) Name() string { return install.ProviderClaudeCode }

// Title implements settings.Provider.
func (*Provider) Title() string { return "Claude Code" }

// Kinds implements settings.Provider.
func (*Provider) Kinds() []settings.KindSpec {
	return []settings.KindSpec{
		{
			Name: install.KindHook, Title: "Hooks", GroupTitle: "Event", Addable: true, Removable: true,
			Fields: []settings.FieldSpec{
				{Name: "event", Label: "Event", Type: "enum", Required: true, Options: Events,
					Help: "When Claude Code runs the hook"},
				{Name: "matcher", Label: "Matcher", Type: "string", Placeholder: "Bash",
					Help: "Which tools or sources it runs for; empty matches all. Some events ignore it."},
				{Name: "if", Label: "If", Type: "string", Placeholder: "Bash(git *)",
					Help: "One permission rule narrowing a tool event further"},
				{Name: "command", Label: "Command", Type: "text", Required: true,
					Help: "Run by Claude Code's shell with the hook input on stdin"},
				{Name: "timeout", Label: "Timeout (seconds)", Type: "int", Placeholder: "60",
					Help: "Empty uses Claude Code's default"},
				{Name: "async", Label: "Run in the background", Type: "bool",
					Help: "Claude Code does not wait for it, so it cannot block or add context"},
			},
		},
		{
			Name: install.KindPermission, Title: "Permissions", GroupTitle: "List", Addable: true, Removable: true,
			Fields: []settings.FieldSpec{
				{Name: "list", Label: "List", Type: "enum", Required: true, Options: permissionLists,
					Help: "deny wins over ask, and ask over allow"},
				{Name: "rule", Label: "Rule", Type: "string", Required: true, Placeholder: "Bash(npm run test *)",
					Help: "A tool name, optionally with a specifier in parentheses"},
			},
		},
		{Name: install.KindPlugin, Title: "Plugins", GroupTitle: "Marketplace", StateInFile: true},
	}
}

// Read implements settings.Provider.
func (p *Provider) Read() (map[string][]byte, error) {
	b, err := settings.ReadFile(p.path)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{p.path: b}, nil
}

// Observe implements settings.Provider.
func (p *Provider) Observe(files map[string][]byte) ([]settings.Item, error) {
	root, err := parse(files[p.path])
	if err != nil {
		return nil, err
	}
	items := hookItems(root)
	items = append(items, permissionItems(root)...)
	items = append(items, pluginItems(root)...)
	return items, nil
}

// Apply implements settings.Provider.
func (p *Provider) Apply(files map[string][]byte, it settings.Item, enabled bool) (map[string][]byte, error) {
	old := files[p.path]
	root, err := parse(old)
	if err != nil {
		return nil, err
	}
	var changed bool
	switch it.Kind {
	case install.KindHook:
		changed, err = applyHook(root, it, enabled)
	case install.KindPermission:
		changed, err = applyPermission(root, it, enabled)
	case install.KindPlugin:
		changed, err = applyPlugin(root, it, enabled)
	default:
		err = settings.Invalidf("claude-code has no kind %q", it.Kind)
	}
	if err != nil || !changed {
		return nil, err
	}
	out, err := marshal(root)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{p.path: out}, nil
}

// Validate implements settings.Provider.
func (*Provider) Validate(kind string, fields map[string]string) (settings.Item, error) {
	switch kind {
	case install.KindHook:
		return validateHook(fields)
	case install.KindPermission:
		return validatePermission(fields)
	}
	return settings.Item{}, settings.Invalidf("%s items cannot be added", kind)
}

// Describe implements settings.Provider.
func (*Provider) Describe(kind string, payload json.RawMessage) (string, string) {
	switch kind {
	case install.KindHook:
		var h hookPayload
		if json.Unmarshal(payload, &h) != nil {
			return "", ""
		}
		return h.Event, describeHook(h)
	case install.KindPermission:
		var r permissionPayload
		if json.Unmarshal(payload, &r) != nil {
			return "", ""
		}
		return r.List, r.Rule
	case install.KindPlugin:
		var pl pluginPayload
		if json.Unmarshal(payload, &pl) != nil {
			return "", ""
		}
		name, market, _ := strings.Cut(pl.Plugin, "@")
		return market, name
	}
	return "", ""
}

// parse decodes settings.json, keeping numbers exactly as written.
func parse(b []byte) (map[string]any, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return root, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	return root, nil
}

// marshal encodes settings.json the way install writes it (two-space
// indent, trailing newline), without escaping <, > and & in commands.
func marshal(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// object returns root[key] as an object: nil when absent, an error for any
// other shape, since settings.json is the user's file to fix.
func object(root map[string]any, key string) (map[string]any, error) {
	switch v := root[key].(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("%w: settings.json: %s is %T, not an object — refusing to rewrite it", settings.ErrInvalid, key, v)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only maps, strings, numbers and bools reach here.
		panic(err)
	}
	return b
}

func jsonInto(b json.RawMessage, v any) error { return json.Unmarshal(b, v) }
