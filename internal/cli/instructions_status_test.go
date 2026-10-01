package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/status"
)

func TestRepositoryInstructions(t *testing.T) {
	for _, tt := range []struct {
		name, baseline, agents, claude, want, fix string
		missing                                   bool
	}{
		{name: "clean", baseline: "@AGENTS.md\n", agents: "Shared rules\n", claude: "@AGENTS.md\n", want: "consistent"},
		{name: "missing guidance", baseline: "@AGENTS.md\n", claude: "@AGENTS.md\n", missing: true, want: "drifted", fix: "git restore --source=HEAD -- AGENTS.md"},
		{name: "edited shared policy", baseline: "@AGENTS.md\n", agents: "Shared rules\n", claude: "@AGENTS.md\n", want: "drifted", fix: "git restore --source=HEAD -- internal/install/agent-instructions.md"},
		{name: "edited guidance", baseline: "@AGENTS.md\n", agents: "Changed rules\n", claude: "@AGENTS.md\n", want: "drifted", fix: "commit intentional changes"},
		{name: "broken import", baseline: "@AGENTS.md\n", agents: "Shared rules\n", claude: "`@AGENTS.md`\n", want: "drifted", fix: "git restore --source=HEAD -- CLAUDE.md"},
		{name: "committed broken import", baseline: "`@AGENTS.md`\n", agents: "Shared rules\n", claude: "`@AGENTS.md`\n", want: "drifted", fix: "standalone @AGENTS.md"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-q")
			write("go.mod", "module github.com/willesq/orchestrator\n")
			write("AGENTS.md", "Shared rules\n")
			write("CLAUDE.md", tt.baseline)
			if err := os.MkdirAll(filepath.Join(dir, "internal", "install"), 0755); err != nil {
				t.Fatal(err)
			}
			write("internal/install/agent-instructions.md", "Shared memory policy\n")
			git("add", ".")
			// Fixture history only; never contacts the developer's signing agent.
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
			if tt.missing {
				if err := os.Remove(filepath.Join(dir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			} else {
				write("AGENTS.md", tt.agents)
			}
			write("CLAUDE.md", tt.claude)
			if tt.name == "edited shared policy" {
				write("internal/install/agent-instructions.md", "Modified policy\n")
			}
			sub := filepath.Join(dir, "sub")
			if err := os.Mkdir(sub, 0755); err != nil {
				t.Fatal(err)
			}
			lines := repositoryInstructionsAt(sub)
			if len(lines) != 3 {
				t.Fatalf("lines=%v", lines)
			}
			found := false
			for _, line := range lines {
				if line.Value == tt.want && (tt.fix == "" || strings.Contains(line.Detail, tt.fix)) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s with fix %q: %+v", tt.want, tt.fix, lines)
			}
			if tt.want == "consistent" && (lines[0].Value != tt.want || lines[1].Value != tt.want) {
				t.Fatalf("clean repo: %+v", lines)
			}
			if tt.missing {
				if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
					t.Fatal("status repaired files")
				}
			}
		})
	}
	if got := repositoryInstructionsAt(t.TempDir()); len(got) != 0 {
		t.Fatalf("non-repository checked: %v", got)
	}
}

func TestGlobalInstructionDriftInHarnessStatus(t *testing.T) {
	isolate(t)
	home, _ := os.UserHomeDir()
	bins, err := installBins("")
	if err != nil {
		t.Fatal(err)
	}
	for _, adapter := range install.AllAdapters() {
		if err := install.BuildPlan(adapter, bins, home).Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	for _, info := range harnessInfo() {
		if info.Pending != 0 || info.Err != "" {
			t.Fatalf("installed: %+v", info)
		}
	}
	for _, tt := range []struct {
		adapter   install.Adapter
		dir, file string
	}{
		{install.Codex{}, ".codex", "AGENTS.md"},
		{install.ClaudeCode{}, ".claude", "CLAUDE.md"},
		{install.Gemini{}, ".gemini", "GEMINI.md"},
	} {
		path := filepath.Join(home, tt.dir, tt.file)
		if err := os.WriteFile(path, []byte("Personal instructions\n"), 0644); err != nil {
			t.Fatal(err)
		}
		infos := harnessInfo()
		for _, info := range infos {
			if info.Name == tt.adapter.Name() && (info.Err != "" || info.Pending != 1) {
				t.Fatalf("entry drift not detected: %+v", info)
			}
		}
		r := status.Collect(context.Background(), status.Deps{Harness: func() []status.HarnessInfo { return infos }})
		var out bytes.Buffer
		r.Render(&out)
		if !strings.Contains(out.String(), "orch install --target "+tt.adapter.Name()+" --apply") {
			t.Fatalf("missing repair: %s", out.String())
		}
		if err := install.BuildPlan(tt.adapter, bins, home).Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		for _, info := range harnessInfo() {
			if info.Pending != 0 || info.Err != "" {
				t.Fatalf("repaired: %+v", info)
			}
		}
	}
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	if err := os.WriteFile(shared, []byte("Tampered local policy\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, info := range harnessInfo() {
		if info.Pending != 1 || info.Err != "" {
			t.Fatalf("local policy drift not detected: %+v", info)
		}
	}
	if err := install.BuildPlan(install.Codex{}, bins, home).Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, info := range harnessInfo() {
		if info.Pending != 0 || info.Err != "" {
			t.Fatalf("local policy repair failed: %+v", info)
		}
	}

	if err := os.Remove(filepath.Join(home, ".config", "orch", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	for _, info := range harnessInfo() {
		if info.Pending == 0 {
			t.Fatalf("missing shared file undetected: %+v", info)
		}
	}
}

func TestRepositoryInstructionStatusRendering(t *testing.T) {
	for _, lines := range [][]status.Line{nil, {{Name: "CLAUDE.md", Value: "drifted", Level: status.LevelWarn, Detail: "restore the import"}}} {
		r := status.Collect(context.Background(), status.Deps{RepositoryInstructions: func() []status.Line { return lines }})
		var out bytes.Buffer
		r.Render(&out)
		if !strings.Contains(out.String(), "repository instructions") {
			t.Fatal(out.String())
		}
		out.Reset()
		if err := r.RenderJSON(&out); err != nil {
			t.Fatal(err)
		}
		var decoded status.Report
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		sec := decoded.Sections[len(decoded.Sections)-1]
		if len(sec.Lines) != len(lines) {
			t.Fatalf("JSON section=%+v", sec)
		}
	}
}
