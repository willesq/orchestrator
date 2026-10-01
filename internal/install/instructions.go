package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const instructionBegin = "<!-- BEGIN orchestrator-managed -->"
const instructionEnd = "<!-- END orchestrator-managed -->"

const memoryInstructions = `## Orchestrator memory

Orchestrator stores durable memory locally across coding agents and sessions.
Use the registered RAG MCP server's rag_search tool to recall relevant prior
decisions and memories before substantive work. Use rag_ingest_text to save
durable user preferences, lessons, and decisions with a stable source_uri.
When updating a memory, reuse its source_uri. Cite source URIs when using memory.
Keep durable memories in RAG rather than harness-specific auto-memory files.
Do not store secrets or facts readily derived from the repository.

MCP server names vary by harness: rag in Claude/Gemini, orch_rag in Codex.
If MCP is unavailable, use orch search and orch ingest text (text on stdin).
Use orch status to diagnose availability. Automatic recall and session capture
depend on the harness's supported hooks; do not assume they ran. Explicit search
and ingest work independently of lifecycle hooks.
`

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

// instructionEdits seeds one shared file from the legacy global CLAUDE.md,
// then installs native imports (Claude/Gemini) or a startup read directive
// (Codex, which does not implement @ imports). Snapshot the legacy source so
// dry-run and apply calculate the same migration before any file is changed.
func instructionEdits(home, target string) []Edit {
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	legacyPath := filepath.Join(home, ".claude", "CLAUDE.md")
	legacy, legacyErr := os.ReadFile(legacyPath)
	if os.IsNotExist(legacyErr) {
		legacyErr = nil
	}
	sharedOld, sharedErr := os.ReadFile(shared)
	if os.IsNotExist(sharedErr) {
		sharedErr = nil
	}
	seed := len(sharedOld) == 0
	// A Codex-only install may already have seeded the shared file while leaving
	// Claude's entry untouched. Migrate it later only if all its rules are present.
	migratedLegacy, migrationErr := managedInstructions(legacy, memoryInstructions)
	legacyPreserved := migrationErr == nil && bytes.Equal(sharedOld, migratedLegacy)
	canonical := Edit{Path: shared, Compute: func(old []byte) ([]byte, string, error) {
		if sharedErr != nil {
			return nil, "", sharedErr
		}
		if len(old) == 0 {
			if legacyErr != nil {
				return nil, "", legacyErr
			}
			old = legacy
		}
		next, err := managedInstructions(old, memoryInstructions)
		return next, "instructions: shared agent-neutral memory policy (seeds existing global CLAUDE.md on first install)", err
	}}
	dir, name := ".claude", "CLAUDE.md"
	body := "@" + shared + "\n"
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
		if target == "claude-code" && (seed || legacyPreserved) && bytes.Equal(old, legacy) {
			// The preceding edit has preserved these instructions in the shared file.
			old = nil
		}
		next, err := managedInstructions(old, body)
		return next, "instructions: load shared policy from " + shared, err
	}}
	return []Edit{canonical, bridge}
}
