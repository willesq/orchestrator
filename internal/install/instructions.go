package install

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const instructionBegin = "<!-- BEGIN orchestrator-managed -->"
const instructionEnd = "<!-- END orchestrator-managed -->"

// memoryInstructions is the version-controlled policy shipped with the CLI.
//
//go:embed agent-instructions.md
var memoryInstructions string

// policyFiles is guidance that applies only to particular work. It installs
// beside the shared policy, which points at each file, so it is read on demand
// instead of loading into every session. Plain files rather than harness
// skills, because every harness reads the shared policy.
//
//go:embed policy/*.md
var policyFiles embed.FS

// policyDir is where the on-demand policy files install, under the home.
func policyDir(home string) string { return filepath.Join(home, ".config", "orch", "policy") }

// policyEdits installs each on-demand policy file, Orchestrator-owned like the
// shared policy.
func policyEdits(home string) []Edit {
	names, _ := fs.Glob(policyFiles, "policy/*.md")
	edits := make([]Edit, 0, len(names))
	for _, src := range names {
		body, _ := policyFiles.ReadFile(src)
		name := path.Base(src)
		edits = append(edits, Edit{Path: filepath.Join(policyDir(home), name), Compute: func([]byte) ([]byte, string, error) {
			return body, "instructions: install on-demand policy " + name, nil
		}})
	}
	return edits
}

// managedInstructions replaces only our block, leaving personal text untouched.
// Reject broken markers rather than risk deleting user instructions.
func managedInstructions(old []byte, body string) ([]byte, error) {
	s := string(old)
	start, end := strings.Index(s, instructionBegin), strings.Index(s, instructionEnd)
	if start >= 0 || end >= 0 {
		if start < 0 || end < start || strings.Count(s, instructionBegin) != 1 || strings.Count(s, instructionEnd) != 1 {
			return nil, fmt.Errorf("malformed orchestrator instruction markers")
		}
		s = s[:start] + instructionBegin + "\n" + body + instructionEnd + s[end+len(instructionEnd):]
	} else {
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "\n" + instructionBegin + "\n" + body + instructionEnd + "\n"
	}
	return []byte(s), nil
}

// instructionEdits installs the embedded repository policy, then wires native
// imports (Claude/Gemini) or an explicit startup read directive (Codex).
// Personal instructions stay in the harness entry files; the shared copy is
// Orchestrator-owned and repaired to the policy shipped with this CLI build.
func instructionEdits(home, target string) []Edit {
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	canonical := Edit{Path: shared, Compute: func(old []byte) ([]byte, string, error) {
		return []byte(memoryInstructions), "instructions: install version-controlled agent-instructions.md shipped with this CLI", nil
	}}
	dir, name := ".claude", "CLAUDE.md"
	body := "@" + strings.ReplaceAll(shared, " ", "\\ ") + "\n"
	if target == "codex" {
		dir, name = ".codex", "AGENTS.md"
		// Codex prefers the override if present. Wire the file it actually loads.
		if info, err := os.Stat(filepath.Join(home, dir, "AGENTS.override.md")); err == nil && info.Size() > 0 {
			name = "AGENTS.override.md"
		}
		body = "At the start of every session, before beginning work, read and follow the shared\n" +
			"user instructions at " + shared + ". Read the entire file; do not treat\n" +
			"this path as an @ import. This is the canonical cross-agent instruction file.\n"
	} else if target == "gemini" {
		dir, name = ".gemini", "GEMINI.md"
	}
	bridge := Edit{Path: filepath.Join(home, dir, name), Compute: func(old []byte) ([]byte, string, error) {
		next, err := managedInstructions(old, body)
		return next, "instructions: load shared policy from " + shared, err
	}}
	return append([]Edit{canonical, bridge}, policyEdits(home)...)
}
