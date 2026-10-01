package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionControlledSharedInstructions(t *testing.T) {
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
	if string(b) != memoryInstructions {
		t.Fatal("installed policy differs from embedded repository source")
	}
	personal, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Personal rule", "Tail rule"} {
		if !strings.Contains(string(personal), want) {
			t.Fatalf("lost personal instruction %q", want)
		}
	}
	if strings.Contains(string(personal), "Old Claude-only policy") {
		t.Fatal("stale managed section retained")
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
	// The local policy must match the embedded source; reinstall repairs drift.
	write(shared, "Modified policy\n")
	for _, adapter := range AllAdapters() {
		pending, err := BuildPlan(adapter, testBins(), home).Pending()
		if err != nil || pending != 1 {
			t.Fatalf("%s pending=%d err=%v", adapter.Name(), pending, err)
		}
	}
	if err := BuildPlan(Codex{}, testBins(), home).Apply(&bytes.Buffer{}, backupDir); err != nil {
		t.Fatal(err)
	}
	for _, adapter := range AllAdapters() {
		pending, err := BuildPlan(adapter, testBins(), home).Pending()
		if err != nil || pending != 0 {
			t.Fatalf("%s pending=%d err=%v", adapter.Name(), pending, err)
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

func TestExistingSharedPolicyBackedUpAndReplaced(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "orch", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Existing shared rule\n"), 0644); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	plan := BuildPlan(Codex{}, testBins(), home)
	if err := plan.Apply(&bytes.Buffer{}, backupDir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != memoryInstructions {
		t.Fatal("did not install embedded policy")
	}
	found := false
	if err := filepath.WalkDir(backupDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(content) == "Existing shared rule\n" {
				found = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("previous shared content was not backed up")
	}
	if n, err := BuildPlan(Codex{}, testBins(), home).Pending(); err != nil || n != 0 {
		t.Fatalf("reinstall pending=%d err=%v", n, err)
	}
}
