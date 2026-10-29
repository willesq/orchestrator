// Package instructions is the settings provider for what orch puts in front
// of agents at startup: each instruction module shipped in the binary (on
// when the managed block of each harness entry file imports it, or for Codex
// lists it), and each Claude Code subagent and skill it ships (on when its
// files are installed under ~/.claude).
//
// It is authoritative: the block, the module files and the marked asset
// files are orch's, so orch.db decides what loads on this machine and every
// poll rewrites what drifted. Text outside the managed block is never
// touched, a file of the user's own is never overwritten or removed, and an
// entry file without the block (a harness orch install never wired) is left
// alone.
package instructions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/settings"
)

// Provider manages the instruction modules of one home.
type Provider struct {
	home    string
	modules func() ([]install.Module, error)
	assets  func() ([]install.ClaudeAsset, error)
	// notRegular holds the module paths the last Read found to be a
	// symlink or other non-regular file. Enforce leaves them alone: the
	// watcher writes without anyone acting, so following a planted link
	// would let it overwrite any file the user owns.
	notRegular map[string]bool
}

// New returns the provider for a home and the modules this build ships.
func New(home string) *Provider {
	return &Provider{home: home, modules: install.Modules, assets: install.ClaudeAssets}
}

// Payload is what a module is listed with: everything the settings UI shows,
// including the full text an agent reads.
type Payload struct {
	Title       string `json:"title"`
	Group       string `json:"group"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body"`
	Path        string `json:"path"`
	Default     bool   `json:"default"`
	// Sensitive modules carry the agents' security policy; clients confirm
	// before turning one off.
	Sensitive bool `json:"sensitive,omitempty"`
	// Files are every file a subagent or skill installs, the main one first.
	Files []string `json:"files,omitempty"`
}

// Name implements settings.Provider.
func (*Provider) Name() string { return install.InstructionsProvider }

// Title implements settings.Provider.
func (*Provider) Title() string { return "Instructions" }

// Kinds implements settings.Provider. StateInFile: a module is listed whether
// or not it is imported, so it is toggled in place and never "removed".
func (*Provider) Kinds() []settings.KindSpec {
	return []settings.KindSpec{
		{Name: install.KindModule, Title: "Instruction modules", GroupTitle: "Topic", StateInFile: true},
		{Name: install.KindAgent, Title: "Subagents", GroupTitle: "Kind", StateInFile: true},
		{Name: install.KindSkill, Title: "Skills", GroupTitle: "Kind", StateInFile: true},
	}
}

// Read implements settings.Provider: every harness entry file and every
// module file, so Enforce may rewrite any of them.
func (p *Provider) Read() (map[string][]byte, error) {
	mods, err := p.modules()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range install.InstructionEntries(p.home) {
		b, err := settings.ReadFile(e.Path)
		if err != nil {
			return nil, err
		}
		files[e.Path] = b
	}
	p.notRegular = map[string]bool{}
	for _, m := range mods {
		path := install.ModulePath(p.home, m.Slug)
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			p.notRegular[path] = true
			files[path] = nil
			continue
		}
		b, err := settings.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files[path] = b
	}
	if err := p.readAssets(files); err != nil {
		return nil, err
	}
	return files, nil
}

// Observe implements settings.Provider. Every shipped module is an item with
// its default state; the stored choice, not the file, decides whether it is on.
func (p *Provider) Observe(map[string][]byte) ([]settings.Item, error) {
	mods, err := p.modules()
	if err != nil {
		return nil, err
	}
	out := make([]settings.Item, 0, len(mods))
	for _, m := range mods {
		payload, err := json.Marshal(Payload{Title: m.Title, Group: m.Group, Description: m.Description,
			Body: m.Body, Path: install.ModulePath(p.home, m.Slug), Default: m.Default, Sensitive: m.Sensitive})
		if err != nil {
			return nil, err
		}
		out = append(out, settings.Item{Kind: install.KindModule, Identity: m.Slug, Payload: payload,
			Enabled: m.Default, Origin: settings.OriginOrch})
	}
	assets, err := p.observeAssets()
	if err != nil {
		return nil, err
	}
	return append(out, assets...), nil
}

// Enforce implements settings.Authoritative: each wired entry file's managed
// block loads exactly the enabled modules, and, once any harness is wired,
// each module file matches what this build ships. An entry with broken
// markers, or a module path that is not a regular file, is reported and
// skipped without holding up the rest.
func (p *Provider) Enforce(files map[string][]byte, enabled func(kind, identity string) (bool, bool)) (settings.Changes, error) {
	mods, err := p.modules()
	if err != nil {
		return settings.Changes{}, err
	}
	on := func(m install.Module) bool {
		if v, known := enabled(install.KindModule, m.Slug); known {
			return v
		}
		return m.Default
	}
	changed := map[string][]byte{}
	c := settings.Changes{Write: changed}
	var problems []error
	wired, claudeWired := false, false
	for _, e := range install.InstructionEntries(p.home) {
		old := files[e.Path]
		if !install.HasManagedBlock(old) {
			continue
		}
		wired = true
		claudeWired = claudeWired || e.Target == "claude-code"
		next, err := install.RenderManagedBlock(old, p.home, e.Target, on)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w — fix the orchestrator-managed markers by hand", e.Path, err))
			continue
		}
		if string(next) != string(old) {
			changed[e.Path] = next
		}
	}
	if !wired {
		// Reason: no harness was wired by orch install, so nothing would load
		// the modules; writing them would only litter an unconfigured home.
		return c, errors.Join(problems...)
	}
	for _, m := range mods {
		path := install.ModulePath(p.home, m.Slug)
		if p.notRegular[path] {
			problems = append(problems, fmt.Errorf("%s is not a regular file; orch owns it and will not write through it — remove it and orch restores it", path))
			continue
		}
		if string(files[path]) != m.Body {
			changed[path] = []byte(m.Body)
		}
	}
	// Reason: only Claude Code loads ~/.claude agents and skills; a home
	// where install never wired it gets none.
	if claudeWired {
		more, err := p.enforceAssets(files, enabled, &c)
		if err != nil {
			return settings.Changes{}, err
		}
		problems = append(problems, more...)
	}
	return c, errors.Join(problems...)
}

// Apply implements settings.Provider. The service writes an authoritative
// provider's changes through Enforce, so Apply never runs.
func (*Provider) Apply(map[string][]byte, settings.Item, bool) (map[string][]byte, error) {
	return nil, fmt.Errorf("%w: instruction modules, subagents and skills are written by Enforce", settings.ErrNotAllowed)
}

// Validate implements settings.Provider; modules ship with the binary.
func (*Provider) Validate(kind string, _ map[string]string) (settings.Item, error) {
	return settings.Item{}, settings.Invalidf("%s items cannot be added here; they ship with orch (internal/install/modules and internal/install/assets)", kind)
}

// Describe implements settings.Provider.
func (*Provider) Describe(_ string, payload json.RawMessage) (string, string) {
	var m Payload
	if json.Unmarshal(payload, &m) != nil {
		return "", ""
	}
	summary := m.Title
	if m.Description != "" {
		summary += ": " + m.Description
	}
	return m.Group, summary
}
