package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/status"
)

// repositoryInstructions is read-only and scoped to this project's Git root,
// including when invoked from a subdirectory or linked worktree. Version control
// is the source of truth for repository guidance; installation owns global files.
func repositoryInstructions() []status.Line {
	cwd, err := os.Getwd()
	if err != nil {
		return []status.Line{{Name: "repository", Value: "unknown", Level: status.LevelWarn, Detail: err.Error()}}
	}
	return repositoryInstructionsAt(cwd)
}

func repositoryInstructionsAt(dir string) []status.Line {
	rootBytes, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	root := strings.TrimSpace(string(rootBytes))
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.Contains(string(module), "module github.com/willesq/orchestrator\n") {
		return nil
	}
	var lines []status.Line
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "internal/install/agent-instructions.md"} {
		line := status.Line{Name: name, Value: "consistent", Level: status.LevelOK, Detail: "matches committed guidance in " + root}
		baseline, err := exec.Command("git", "-C", root, "show", "HEAD:"+name).Output()
		if err != nil || len(bytes.TrimSpace(baseline)) == 0 {
			line.Value, line.Level = "unknown", status.LevelWarn
			line.Detail = "no committed guidance available; update this checkout to a revision containing " + name
		} else {
			current, err := os.ReadFile(filepath.Join(root, name))
			switch {
			case err != nil && !os.IsNotExist(err):
				line.Value, line.Level, line.Detail = "unknown", status.LevelWarn, err.Error()
			case err != nil || !bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(baseline)):
				line.Value, line.Level = "drifted", status.LevelWarn
				line.Detail = fmt.Sprintf("missing or differs from HEAD; review local edits, then from %s restore committed guidance: git restore --source=HEAD -- %s (discards local edits); or commit intentional changes", root, name)
			}
			// A committed broken bridge is still broken; matching HEAD alone is
			// insufficient. The import must be a real directive, not fenced text.
			if name == "CLAUDE.md" && !bytes.Equal(bytes.TrimSpace(baseline), []byte("@AGENTS.md")) && line.Value == "consistent" {
				line.Value, line.Level = "drifted", status.LevelWarn
				line.Detail = "CLAUDE.md must contain the standalone @AGENTS.md import; replace its contents with @AGENTS.md and commit the correction"
			}
		}
		lines = append(lines, line)
	}
	return lines
}
