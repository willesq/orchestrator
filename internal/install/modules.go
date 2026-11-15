package install

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// The settings provider and kind for instruction modules. The
// orch-instructions settings provider stores modules under these names, so a
// toggle made in the app is the toggle install reads.
const (
	InstructionsProvider = "orch-instructions"
	KindModule           = "module"
)

// moduleFiles are the instruction modules: one file per intent, each a piece
// of the instructions agents load at startup that a machine can turn on or
// off. Adding a module is adding a file here; nothing else changes.
//
//go:embed modules/*.md
var moduleFiles embed.FS

// Module is one shipped instruction module.
type Module struct {
	// Slug is the file name without .md; it is the setting's identity.
	Slug        string
	Title       string
	Group       string
	Description string
	// Order places the module's import in the managed block.
	Order int
	// Default is whether a machine that never toggled it loads it.
	Default bool
	// Sensitive modules carry the agents' security policy: turning one off
	// asks for confirmation and orch status names it.
	Sensitive bool
	// Body is the text installed for agents, without the frontmatter.
	Body string
}

// moduleMeta is a module file's frontmatter.
type moduleMeta struct {
	Title       string `yaml:"title"`
	Group       string `yaml:"group"`
	Order       int    `yaml:"order"`
	Description string `yaml:"description"`
	Default     *bool  `yaml:"default"`
	Sensitive   bool   `yaml:"sensitive"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

var (
	modulesOnce sync.Once
	modules     []Module
	modulesErr  error
)

// Modules returns the shipped modules in import order. A malformed module
// file is a build defect that TestModulesParse catches.
func Modules() ([]Module, error) {
	modulesOnce.Do(func() { modules, modulesErr = parseModules(moduleFiles) })
	return modules, modulesErr
}

func parseModules(fsys fs.FS) ([]Module, error) {
	names, err := fs.Glob(fsys, "modules/*.md")
	if err != nil {
		return nil, err
	}
	out := make([]Module, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		m, err := parseModule(strings.TrimSuffix(path.Base(name), ".md"), raw)
		if err != nil {
			return nil, fmt.Errorf("instruction module %s: %w", name, err)
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// parseModule splits a module file into its frontmatter and the body agents
// read. The frontmatter is for orch and the settings UI only.
func parseModule(slug string, raw []byte) (Module, error) {
	if !slugRe.MatchString(slug) {
		return Module{}, fmt.Errorf("file name must be lowercase letters, digits and dashes")
	}
	const fence = "---\n"
	s := string(raw)
	if !strings.HasPrefix(s, fence) {
		return Module{}, fmt.Errorf("missing frontmatter")
	}
	head, body, ok := strings.Cut(s[len(fence):], "\n"+fence)
	if !ok {
		return Module{}, fmt.Errorf("unterminated frontmatter")
	}
	var meta moduleMeta
	dec := yaml.NewDecoder(strings.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&meta); err != nil {
		return Module{}, fmt.Errorf("frontmatter: %w", err)
	}
	body = strings.TrimLeft(body, "\n")
	if meta.Title == "" || strings.TrimSpace(body) == "" {
		return Module{}, fmt.Errorf("needs a title and a body")
	}
	m := Module{Slug: slug, Title: meta.Title, Group: meta.Group, Description: meta.Description,
		Order: meta.Order, Default: true, Sensitive: meta.Sensitive, Body: body}
	if meta.Default != nil {
		m.Default = *meta.Default
	}
	return m, nil
}

// ModuleDir is where modules install, under the home.
func ModuleDir(home string) string { return filepath.Join(home, ".config", "orch", "modules") }

// ModulePath is where one module installs.
func ModulePath(home, slug string) string { return filepath.Join(ModuleDir(home), slug+".md") }

// moduleEdits installs every shipped module file, on or off: the file is
// Orchestrator-owned like the shared policy, and only the import decides
// whether an agent loads it.
func moduleEdits(home string) []Edit {
	mods, err := Modules()
	if err != nil {
		return []Edit{{Path: ModuleDir(home), Compute: func([]byte) ([]byte, string, error) { return nil, "", err },
			Reverse: func([]byte) (Undo, error) { return Undo{}, err }}}
	}
	edits := make([]Edit, 0, len(mods))
	for _, m := range mods {
		body := []byte(m.Body)
		edits = append(edits, Edit{Path: ModulePath(home, m.Slug), Compute: func([]byte) ([]byte, string, error) {
			return body, "instructions: install module " + m.Slug, nil
		}, Reverse: func(cur []byte) (Undo, error) {
			return undoOwnedFile(cur, "instructions: module "+m.Slug, nil)
		}})
	}
	return edits
}

// SharedPolicyPath is the installed shared policy.
func SharedPolicyPath(home string) string { return filepath.Join(home, ".config", "orch", "AGENTS.md") }

// InstructionEntry is a harness entry file that carries orch's managed block.
type InstructionEntry struct {
	Target string
	Path   string
}

// InstructionEntries lists the entry file of each harness, whether or not it
// exists. Codex prefers a nonempty override, so that is the file it loads.
func InstructionEntries(home string) []InstructionEntry {
	return []InstructionEntry{
		{"claude-code", filepath.Join(home, ".claude", "CLAUDE.md")},
		{"codex", codexEntry(home)},
		{"gemini", filepath.Join(home, ".gemini", "GEMINI.md")},
	}
}

// ModuleEnabled reports whether a module is on, given the stored toggles:
// the stored choice when there is one, the module's default otherwise.
type ModuleEnabled func(m Module) bool

// bridgeBody is the managed block for a harness: the shared policy, then
// every enabled module. Claude Code and Gemini import each file; Codex has
// no imports, so it is told to read them.
func bridgeBody(home, target string, on ModuleEnabled) (string, error) {
	mods, err := Modules()
	if err != nil {
		return "", err
	}
	shared := SharedPolicyPath(home)
	var b strings.Builder
	if target == "codex" {
		b.WriteString("At the start of every session, before beginning work, read and follow the shared\n" +
			"user instructions at " + shared + ". Read the entire file; do not treat\n" +
			"this path as an @ import. This is the canonical cross-agent instruction file.\n")
		first := true
		for _, m := range mods {
			if !on(m) {
				continue
			}
			if first {
				b.WriteString("Then read and follow each of these instruction modules, in order:\n")
				first = false
			}
			b.WriteString("- " + ModulePath(home, m.Slug) + "\n")
		}
		return b.String(), nil
	}
	b.WriteString("@" + escapeImport(shared) + "\n")
	for _, m := range mods {
		if on(m) {
			b.WriteString("@" + escapeImport(ModulePath(home, m.Slug)) + "\n")
		}
	}
	return b.String(), nil
}

func escapeImport(p string) string { return strings.ReplaceAll(p, " ", "\\ ") }

// HasManagedBlock reports whether an entry file carries orch's block, which
// is how the settings watcher knows install wired that harness.
func HasManagedBlock(b []byte) bool { return bytes.Contains(b, []byte(instructionBegin)) }

// RenderManagedBlock returns an entry file with its managed block rewritten
// to load the shared policy and the enabled modules. Text outside the block
// is kept; broken markers are refused.
func RenderManagedBlock(old []byte, home, target string, on ModuleEnabled) ([]byte, error) {
	body, err := bridgeBody(home, target, on)
	if err != nil {
		return nil, err
	}
	return managedInstructions(old, body)
}
