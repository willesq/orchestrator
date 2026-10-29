package install

import (
	"embed"
	"fmt"
	"io/fs"
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

// instructionEdits installs the embedded repository policy and modules, then
// wires native imports (Claude/Gemini) or an explicit startup read directive
// (Codex) for the shared policy and every module this machine has on.
// Personal instructions stay in the harness entry files; the shared copy and
// the modules are Orchestrator-owned and repaired to what this build ships.
func instructionEdits(home, target string) []Edit {
	shared := SharedPolicyPath(home)
	canonical := Edit{Path: shared, Compute: func(old []byte) ([]byte, string, error) {
		return []byte(memoryInstructions), "instructions: install version-controlled agent-instructions.md shipped with this CLI", nil
	}}
	entry := ""
	for _, e := range InstructionEntries(home) {
		if e.Target == target {
			entry = e.Path
		}
	}
	bridge := Edit{Path: entry, Compute: func(old []byte) ([]byte, string, error) {
		// Reason: a module turned off in orch stays out of the block. A store
		// that cannot be read stops the plan rather than being taken as
		// "nothing is off", which would turn every module back on.
		toggles, err := loadToggles()
		if err != nil {
			return nil, "", fmt.Errorf("%w — refusing to write %s, so instruction modules turned off in orch stay off", err, entry)
		}
		next, err := RenderManagedBlock(old, home, target, ModuleOn(toggles))
		return next, "instructions: load shared policy and enabled modules from " + filepath.Dir(shared), err
	}}
	edits := append([]Edit{canonical, bridge}, moduleEdits(home)...)
	return append(edits, policyEdits(home)...)
}
