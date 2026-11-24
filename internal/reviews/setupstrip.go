package reviews

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// reviewerAgents are the agents a review runs as or fans out to. A project
// agent of the same name would take precedence over the one orch installs, so
// a repo could replace its own reviewer and write its verdict.
var reviewerAgents = map[string]bool{
	Agent: true, "adversarial-reviewer": true, "adversarial-qa": true, "exploit-finder": true,
}

// keptKeys are the frontmatter keys an agent, skill or command may carry when
// the repo's settings are off. Anything else (hooks, permissionMode,
// mcpServers, allowed-tools, and keys added later) can make the session run
// the repo's code or widen what it may do, so a file with one is removed.
var keptKeys = map[string]bool{
	"name": true, "description": true, "model": true, "tools": true, "disallowedTools": true,
	"color": true, "argument-hint": true, "license": true, "version": true,
}

// maxFrontmatterRead bounds what is read of each file; frontmatter that does
// not close within it counts as unreadable.
const maxFrontmatterRead = 64 << 10

// stripSetup removes from a setup checkout what would let the repo take over
// the review or, with settings off, run its own code:
//   - always, any project agent named like one of reviewerAgents, and any
//     agent, skill or command whose frontmatter cannot be read for certain;
//   - without settings, the settings and MCP files, and any agent, skill or
//     command whose frontmatter has a key outside keptKeys.
//
// Everything goes through an os.Root, so a path the repo makes point
// elsewhere can never reach outside the checkout.
func stripSetup(dir string, settings bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if !settings {
		for _, f := range projectSettings {
			if err := removeIn(root, f); err != nil {
				return err
			}
		}
	}
	for _, sub := range []string{".claude/agents", ".claude/skills", ".claude/commands"} {
		err := fs.WalkDir(root.FS(), sub, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if absent(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			head, err := readHead(root, p)
			if err != nil {
				return err
			}
			if !keep(head, strings.TrimSuffix(path.Base(p), ".md"), sub == ".claude/agents", settings) {
				return removeIn(root, p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// keep decides whether a Markdown file may stay in a setup checkout.
func keep(content, stem string, agent, settings bool) bool {
	keys, ok := frontmatter(content)
	if !ok {
		return false
	}
	if agent {
		name, _ := keys["name"].(string)
		for reserved := range reviewerAgents {
			if strings.EqualFold(strings.TrimSpace(name), reserved) || strings.EqualFold(stem, reserved) {
				return false
			}
		}
		if _, isString := keys["name"].(string); keys["name"] != nil && !isString {
			return false
		}
	}
	if !settings {
		for k := range keys {
			if !keptKeys[k] {
				return false
			}
		}
	}
	return true
}

func readHead(root *os.Root, p string) (string, error) {
	f, err := root.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFrontmatterRead))
	return string(b), err
}

// removeIn removes p inside root; one already gone, or under a path that is
// not a directory (a symlink checked out as a file), is not an error.
func removeIn(root *os.Root, p string) error {
	if err := root.Remove(p); err != nil && !absent(err) {
		return err
	}
	return nil
}

func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// frontmatter parses a Markdown file's YAML frontmatter. ok is false when the
// file opens frontmatter that does not close or does not parse as one
// mapping: what Claude Code would make of it cannot be known, so it fails
// closed. A file without frontmatter has no keys.
func frontmatter(s string) (keys map[string]any, ok bool) {
	s = strings.ReplaceAll(strings.TrimPrefix(s, "\uFEFF"), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) || strings.TrimSpace(lines[first]) != "---" {
		return map[string]any{}, true
	}
	for i := first + 1; i < len(lines); i++ {
		// Only "---" closes it: YAML's "..." might not end it for Claude Code,
		// which could then read keys orch never saw.
		if strings.TrimSpace(lines[i]) == "---" {
			// Exactly one YAML document: a second, after "...", would be
			// keys this parse never sees.
			dec := yaml.NewDecoder(strings.NewReader(strings.Join(lines[first+1:i], "\n")))
			keys = map[string]any{}
			if err := dec.Decode(&keys); err != nil && !errors.Is(err, io.EOF) {
				return nil, false
			}
			var more any
			if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
				return nil, false
			}
			return keys, true
		}
	}
	return nil, false
}
