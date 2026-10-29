package install

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Kinds of the Claude Code assets a machine can turn on or off, stored under
// the orch-instructions settings provider beside instruction modules.
const (
	KindAgent = "agent"
	KindSkill = "skill"
)

// ClaudeAsset is one shipped subagent or skill with every file it installs.
// It is the unit a toggle turns on or off.
type ClaudeAsset struct {
	Kind        string
	Name        string
	Title       string
	Description string
	// Sensitive assets carry security review duties; turning one off asks
	// for confirmation and orch status names it.
	Sensitive bool
	// Files are relative to ~/.claude, the agent or SKILL.md file first.
	Files []AssetFile
	// Dirs are the directories under ~/.claude only this asset uses, deepest
	// first, so they can be removed once the asset is off and they are empty.
	Dirs []string
}

// AssetFile is one installed file of an asset.
type AssetFile struct {
	Rel  string // relative to ~/.claude
	Body []byte
	// Pinned files are agent helper scripts a permission rule may
	// pre-authorize by path (exploit-finder-sandbox.sh). Whatever sits at
	// that path runs without a prompt, so it is never the user's: on, it is
	// always the shipped copy; off, anything there is removed.
	Pinned bool
}

// Main is the asset's agent or SKILL.md file.
func (a ClaudeAsset) Main() AssetFile { return a.Files[0] }

var (
	claudeAssetsOnce sync.Once
	claudeAssetsList []ClaudeAsset
	claudeAssetsErr  error
)

// ClaudeAssets lists the shipped subagents and skills, agents first, each by
// name. Adding one is adding its files under internal/install/assets/claude.
func ClaudeAssets() ([]ClaudeAsset, error) {
	claudeAssetsOnce.Do(func() {
		sub, err := fs.Sub(assets, "assets/claude")
		if err == nil {
			claudeAssetsList, claudeAssetsErr = parseClaudeAssets(sub)
		} else {
			claudeAssetsErr = err
		}
	})
	return claudeAssetsList, claudeAssetsErr
}

// parseClaudeAssets groups an asset tree into agents and skills. An agent is
// agents/<name>.md plus any agents/<name>-* helper and agents/_shared/<name>-*
// directory; a skill is everything under skills/<name>/. A file no asset owns
// is a build defect.
func parseClaudeAssets(fsys fs.FS) ([]ClaudeAsset, error) {
	var all []string
	if err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Reason: all: also embeds dot-files a working tree picks up
		// (.DS_Store, editor swap files); none is an asset.
		if strings.HasPrefix(d.Name(), ".") && p != "." {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			all = append(all, p)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(all)
	byKey := map[string]*ClaudeAsset{}
	var order []string
	add := func(kind, name, rel string, main bool, fsys fs.FS) error {
		key := kind + "/" + name
		a := byKey[key]
		if a == nil {
			a = &ClaudeAsset{Kind: kind, Name: name}
			byKey[key] = a
			order = append(order, key)
		}
		body, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return err
		}
		f := AssetFile{Rel: rel, Body: body, Pinned: kind == KindAgent && path.Dir(rel) == "agents" && !strings.HasSuffix(rel, ".md")}
		if main {
			a.Files = append([]AssetFile{f}, a.Files...)
		} else {
			a.Files = append(a.Files, f)
		}
		return nil
	}
	var agents []string
	for _, p := range all {
		if dir, file := path.Split(p); dir == "agents/" && strings.HasSuffix(file, ".md") {
			agents = append(agents, strings.TrimSuffix(file, ".md"))
		}
	}
	// Reason: longest first, so a helper of "exploit-finder" is never taken
	// by a shorter agent name that happens to prefix it.
	sort.Slice(agents, func(i, j int) bool { return len(agents[i]) > len(agents[j]) })
	owner := func(rest string) string {
		for _, a := range agents {
			if strings.HasPrefix(rest, a+"-") {
				return a
			}
		}
		return ""
	}
	for _, p := range all {
		parts := strings.Split(p, "/")
		switch {
		case parts[0] == "skills" && len(parts) >= 3:
			if err := add(KindSkill, parts[1], p, p == path.Join("skills", parts[1], "SKILL.md"), fsys); err != nil {
				return nil, err
			}
		case parts[0] == "agents" && len(parts) == 2 && strings.HasSuffix(p, ".md"):
			if err := add(KindAgent, strings.TrimSuffix(parts[1], ".md"), p, true, fsys); err != nil {
				return nil, err
			}
		case parts[0] == "agents" && len(parts) == 2:
			name := owner(parts[1])
			if name == "" {
				return nil, fmt.Errorf("asset %s belongs to no agent: name it <agent>-…", p)
			}
			if err := add(KindAgent, name, p, false, fsys); err != nil {
				return nil, err
			}
		case parts[0] == "agents" && parts[1] == "_shared" && len(parts) >= 4:
			name := owner(parts[2])
			if name == "" {
				return nil, fmt.Errorf("asset %s belongs to no agent: name its directory <agent>-…", p)
			}
			if err := add(KindAgent, name, p, false, fsys); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("asset %s is neither under agents/ nor skills/<name>/", p)
		}
	}
	out := make([]ClaudeAsset, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		main := a.Main().Rel
		if (a.Kind == KindSkill && path.Base(main) != "SKILL.md") || (a.Kind == KindAgent && path.Dir(main) != "agents") {
			return nil, fmt.Errorf("%s %s has no %s", a.Kind, a.Name, map[string]string{KindAgent: "agents/" + a.Name + ".md", KindSkill: "SKILL.md"}[a.Kind])
		}
		for _, f := range a.Files {
			if !bytes.Contains(f.Body, []byte(managedMarker)) {
				return nil, fmt.Errorf("asset %s lacks the %q marker", f.Rel, managedMarker)
			}
		}
		a.Title, a.Description = frontmatterNameDesc(a.Main().Body, a.Name)
		a.Sensitive = a.Kind == KindAgent && sensitiveAgents[a.Name]
		a.Dirs = assetDirs(*a)
		out = append(out, *a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == KindAgent
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// sensitiveAgents are the review agents a security workflow depends on.
var sensitiveAgents = map[string]bool{"security-audit": true, "adversarial-reviewer": true, "exploit-finder": true}

// assetDirs lists the directories only this asset uses, deepest first: a
// skill's whole tree, an agent's _shared directories.
func assetDirs(a ClaudeAsset) []string {
	seen := map[string]bool{}
	for _, f := range a.Files {
		for d := path.Dir(f.Rel); d != "." && d != "agents" && d != "agents/_shared" && d != "skills"; d = path.Dir(d) {
			seen[d] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if ci, cj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/"); ci != cj {
			return ci > cj
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

// frontmatterNameDesc reads name and description from a file's YAML
// frontmatter, falling back to the asset name.
func frontmatterNameDesc(body []byte, fallback string) (string, string) {
	s := string(body)
	if !strings.HasPrefix(s, "---\n") {
		return fallback, ""
	}
	head, _, ok := strings.Cut(s[4:], "\n---\n")
	if !ok {
		return fallback, ""
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(head), &meta) != nil {
		// Reason: Claude Code reads frontmatter leniently, so a skill whose
		// one-line description holds ": " works there but is not strict YAML;
		// read such single-line values as plain text.
		meta.Name, meta.Description = "", ""
		for _, line := range strings.Split(head, "\n") {
			if v, ok := strings.CutPrefix(line, "name: "); ok {
				meta.Name = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "description: "); ok {
				meta.Description = v
			}
		}
	}
	if meta.Name == "" {
		meta.Name = fallback
	}
	return meta.Name, strings.Join(strings.Fields(meta.Description), " ")
}

// markerInsert is how an asset copied from a user's own file was marked: the
// marker line and a blank line, after the frontmatter or at the top.
var markerInsert = func() string {
	return "<!-- " + managedMarker + " — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->\n\n"
}()

// IsAdoptable reports whether an unmarked file on disk is exactly the shipped
// body without its marker: the user's own copy orch now ships, which orch can
// take over without losing a byte of it.
func IsAdoptable(onDisk, shipped []byte) bool {
	unmarked := bytes.Replace(shipped, []byte(markerInsert), nil, 1)
	return len(onDisk) > 0 && !bytes.Equal(unmarked, shipped) && bytes.Equal(onDisk, unmarked)
}

// DisplayBody is an asset file as a person reads it: without the YAML
// frontmatter Claude Code consumes and without orch's marker line.
func DisplayBody(b []byte) string {
	s := strings.Replace(string(b), markerInsert, "", 1)
	if strings.HasPrefix(s, "---\n") {
		if _, rest, ok := strings.Cut(s[4:], "\n---\n"); ok {
			s = rest
		}
	}
	return strings.TrimLeft(s, "\n")
}

// IsManaged reports whether a file carries orch's marker.
func IsManaged(b []byte) bool { return bytes.Contains(b, []byte(managedMarker)) }

// ClaudeAssetPath is where an asset file installs.
func ClaudeAssetPath(home, rel string) string {
	return filepath.Join(home, ".claude", filepath.FromSlash(rel))
}

// AssetEnabled reports whether a shipped agent or skill is on, given the
// stored toggles; every asset defaults to on.
func AssetEnabled(t interface {
	State(provider, kind, identity string) (bool, bool)
}) func(ClaudeAsset) bool {
	return func(a ClaudeAsset) bool {
		if on, known := t.State(InstructionsProvider, a.Kind, a.Name); known {
			return on
		}
		return true
	}
}
