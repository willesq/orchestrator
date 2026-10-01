package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedInstructionsMigration(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".claude", "CLAUDE.md")
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(legacy, "Personal rule\n"+instructionBegin+"\nOld Claude-only policy\n"+instructionEnd+"\nTail rule\n")
	write(filepath.Join(home, ".codex", "AGENTS.override.md"), "Codex-specific rule\n")
	backupDir := t.TempDir()
	for _, adapter := range AllAdapters() {
		plan := BuildPlan(adapter, testBins(), home)
		if err := plan.Print(&bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if adapter.Name() == "claude-code" {
			if _, err := os.Stat(filepath.Join(home, ".config", "orch", "AGENTS.md")); !os.IsNotExist(err) {
				t.Fatal("dry-run wrote shared instructions")
			}
		}
		if err := plan.Apply(&bytes.Buffer{}, backupDir); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	b, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Personal rule", "Tail rule", "rag_search", "rag_ingest_text"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("shared instructions missing %q", want)
		}
	}
	if strings.Contains(string(b), "Old Claude-only policy") {
		t.Fatal("stale policy survived migration")
	}
	for _, path := range []string{legacy, filepath.Join(home, ".codex", "AGENTS.override.md"), filepath.Join(home, ".gemini", "GEMINI.md")} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), shared) {
			t.Errorf("%s missing shared reference", path)
		}
	}
	// Edits to the canonical personal rules survive reinstall and all harnesses
	// remain clean without copying those rules into their entry files.
	write(shared, "New shared preference\n"+string(b))
	for _, adapter := range AllAdapters() {
		pending, err := BuildPlan(adapter, testBins(), home).Pending()
		if err != nil || pending != 0 {
			t.Errorf("%s pending=%d err=%v", adapter.Name(), pending, err)
		}
	}
	b, _ = os.ReadFile(filepath.Join(home, ".codex", "AGENTS.override.md"))
	if !strings.Contains(string(b), "Codex-specific rule") {
		t.Fatal("lost Codex-specific instructions")
	}
}

func TestManagedInstructionsMalformed(t *testing.T) {
	for _, old := range []string{instructionBegin, instructionEnd, instructionEnd + instructionBegin, instructionBegin + instructionBegin + instructionEnd} {
		if _, err := managedInstructions([]byte(old), memoryInstructions); err == nil {
			t.Errorf("accepted broken markers %q", old)
		}
	}
}

func TestExistingSharedInstructionsPreserved(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "orch", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Existing shared rule\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(Codex{}, testBins(), home)
	if err := plan.Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "Existing shared rule") {
		t.Fatal("lost existing instructions")
	}
	if n, err := BuildPlan(Codex{}, testBins(), home).Pending(); err != nil || n != 0 {
		t.Fatalf("reinstall pending=%d err=%v", n, err)
	}
}
