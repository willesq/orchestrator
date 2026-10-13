package install

import (
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionFootprints(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	write(t, shared, strings.Repeat("s", 400))
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "personal\n@"+shared+"\n@RTK.md\nsee @~/notes/my\\ rules.md for more\n@~/.claude/CLAUDE.md\n")
	write(t, filepath.Join(home, ".claude", "RTK.md"), "rtk @deep1.md")
	write(t, filepath.Join(home, "notes", "my rules.md"), "rules")
	// A chain longer than Claude follows: only maxImportDepth hops count.
	for i := 1; i <= maxImportDepth+2; i++ {
		write(t, filepath.Join(home, ".claude", "deep"+string(rune('0'+i))+".md"), "d @deep"+string(rune('0'+i+1))+".md")
	}
	write(t, filepath.Join(home, ".codex", "AGENTS.md"), "read and follow "+shared+" at the start of every session")
	write(t, filepath.Join(home, ".gemini", "GEMINI.md"), "gemini only")

	got := map[string]Footprint{}
	for _, f := range InstructionFootprints(home) {
		got[f.Harness] = f
	}
	claude := got["claude-code"]
	for _, want := range []string{"CLAUDE.md", "AGENTS.md", "RTK.md", "my rules.md", "deep1.md", "deep4.md"} {
		if !slices.ContainsFunc(claude.Files, func(p string) bool { return filepath.Base(p) == want }) {
			t.Errorf("claude files missing %s: %v", want, claude.Files)
		}
	}
	if slices.ContainsFunc(claude.Files, func(p string) bool { return filepath.Base(p) == "deep5.md" }) {
		t.Errorf("imports followed past depth %d: %v", maxImportDepth, claude.Files)
	}
	if n := strings.Count(strings.Join(claude.Files, "\n"), "CLAUDE.md"); n != 1 {
		t.Errorf("a self-import was counted %d times", n)
	}
	if codex := got["codex"]; len(codex.Files) != 2 || codex.Bytes < 400 {
		t.Errorf("codex should count its entry and the shared policy it is told to read: %+v", codex)
	}
	if gem := got["gemini"]; len(gem.Files) != 1 {
		t.Errorf("gemini = %+v", gem)
	}
	if (Footprint{Bytes: 4001}).Tokens() != 1001 {
		t.Errorf("token estimate rounds wrong")
	}
}

func TestCodexOverrideIsMeasured(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex", "AGENTS.md"), "ignored")
	write(t, filepath.Join(home, ".codex", "AGENTS.override.md"), "the file codex loads")
	fps := InstructionFootprints(home)
	if len(fps) != 1 || filepath.Base(fps[0].Files[0]) != "AGENTS.override.md" {
		t.Errorf("footprints = %+v", fps)
	}
	if len(InstructionFootprints(t.TempDir())) != 0 {
		t.Errorf("a home with no harness files reported footprints")
	}
}

func TestPolicyFilesInstall(t *testing.T) {
	home := t.TempDir()
	plan := Plan{Target: "claude-code", Edits: instructionEdits(home, "claude-code")}
	if err := plan.Apply(io.Discard, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(policyFiles, "policy/*.md")
	if len(names) == 0 {
		t.Fatal("no policy files embedded")
	}
	for _, src := range names {
		want, _ := policyFiles.ReadFile(src)
		got, err := os.ReadFile(filepath.Join(policyDir(home), path.Base(src)))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s not installed verbatim (err %v)", src, err)
		}
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Errorf("re-plan after apply: %d pending, %v", n, err)
	}
	write(t, filepath.Join(policyDir(home), "pr-review.md"), "edited by hand")
	if n, _ := plan.Pending(); n != 1 {
		t.Errorf("a hand-edited policy file should show as drift, got %d pending", n)
	}
}

func TestSharedPolicyPointsOnlyAtShippedFiles(t *testing.T) {
	refs := regexp.MustCompile("`(?:policy/)?([a-z-]+\\.md)`").FindAllStringSubmatch(memoryInstructions, -1)
	var pointed []string
	for _, m := range refs {
		if m[1] != "AGENTS.md" {
			pointed = append(pointed, m[1])
		}
	}
	if len(pointed) == 0 {
		t.Fatal("the shared policy points at no on-demand files")
	}
	for _, name := range pointed {
		if _, err := policyFiles.ReadFile("policy/" + name); err != nil {
			t.Errorf("shared policy points at %s, which is not shipped", name)
		}
	}
	names, _ := fs.Glob(policyFiles, "policy/*.md")
	for _, src := range names {
		if !slices.Contains(pointed, path.Base(src)) {
			t.Errorf("%s ships but the shared policy never points at it", src)
		}
	}
}
