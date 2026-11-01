package agentrun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/redact"
)

// activityTail is how much of a transcript's end is read for its activity, so
// a long session costs the same each time it is polled.
const activityTail = 4 << 20

// activityTextLimit caps one step's text.
const activityTextLimit = 240

// ActivityStep is one thing a session did.
type ActivityStep struct {
	At   string // RFC 3339, as the transcript recorded it
	Kind string // read | edit | run | agent | other
	Text string // the command, path or pattern, redacted and capped
	Risk string // what makes it risky, or ""
}

// Activity is what a task's session has done so far.
type Activity struct {
	Available bool
	Steps     []ActivityStep
	Total     int
}

// riskyCommands are what the user would want to see an agent do, each matched
// where a shell command starts, so a commit message that mentions one does not
// count. A command hidden inside a script or `sh -c` is not seen: this shows
// what happened, and is not the thing that prevents it.
var riskyCommands = []struct {
	label string
	re    *regexp.Regexp
}{
	{"force push", regexp.MustCompile(gitCommand + `push\b[^\n;&|]*(\s--force\S*|\s-f\b|\s\+\S)`)},
	{"git push", regexp.MustCompile(gitCommand + `push\b`)},
	{"hard reset", regexp.MustCompile(gitCommand + `reset\b[^\n;&|]*\s--hard\b`)},
	{"merges a PR", regexp.MustCompile(commandStart + `gh\s+pr\s+merge\b`)},
	{"closes a PR", regexp.MustCompile(commandStart + `gh\s+pr\s+close\b`)},
	{"posts to GitHub", regexp.MustCompile(commandStart + `gh\s+(pr|issue)\s+(comment|review|edit|ready|create)\b`)},
	{"calls the GitHub API to write", regexp.MustCompile(commandStart + `gh\s+api\b[^\n;&|]*(\s-X\s*|\s--method[ =])(POST|PUT|PATCH|DELETE)\b`)},
	{"deletes files", regexp.MustCompile(commandStart + `rm\s+(-\w*[rR]\w*\s+-?\w*[fF]\w*|-\w*[fF]\w*\s+-?\w*[rR]\w*|-\w*[rR]\w*[fF]\w*|-\w*[fF]\w*[rR]\w*)\b`)},
}

const (
	commandStart = `(?:^|[;&|(\n]\s*|\bcd\s+\S+\s*&&\s*)(?:\w+=\S*\s+)*`
	gitCommand   = commandStart + `git(?:\s+-[cC]\s+\S+|\s+--\S+)*\s+`
)

// Risk names what makes a shell command risky, or "".
func Risk(command string) string {
	for _, r := range riskyCommands {
		if r.re.MatchString(command) {
			return r.label
		}
	}
	return ""
}

// ReadActivity reads what the task's Claude Code session under home has done.
// The newest limit steps are kept, plus up to limit earlier risky ones.
// A task with no transcript (a Codex run, or one that has not started) is not
// an error: it is just not Available.
func ReadActivity(home, taskID string, limit int) (Activity, error) {
	if home == "" {
		return Activity{}, nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", SessionID(taskID)+".jsonl"))
	var newest string
	var newestInfo os.FileInfo
	for _, m := range matches {
		// Only a regular file: opening a FIFO left at this path would block.
		if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() && (newestInfo == nil || info.ModTime().After(newestInfo.ModTime())) {
			newest, newestInfo = m, info
		}
	}
	if newest == "" {
		return Activity{}, nil
	}
	f, err := os.Open(newest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Activity{}, nil
		}
		return Activity{}, err
	}
	defer func() { _ = f.Close() }()
	if offset := newestInfo.Size() - activityTail; offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return Activity{}, err
		}
	}
	steps, err := parseActivity(f, newestInfo.Size() > activityTail)
	if err != nil {
		return Activity{}, err
	}
	return Activity{Available: true, Steps: trimSteps(steps, limit), Total: len(steps)}, nil
}

// parseActivity reads a transcript's tool calls. The transcript is being
// written while it is read, and past the tail's start the first line is
// partial, so lines that do not parse are skipped.
func parseActivity(r io.Reader, partialFirst bool) ([]ActivityStep, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var steps []ActivityStep
	for first := true; ; first = false {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && (!first || !partialFirst) && bytes.Contains(line, []byte(`"tool_use"`)) {
			steps = append(steps, parseActivityLine(line)...)
		}
		if err == io.EOF {
			return steps, nil
		}
		if err != nil {
			return steps, err
		}
	}
}

func parseActivityLine(line []byte) []ActivityStep {
	var entry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
		return nil
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return nil
	}
	var steps []ActivityStep
	for _, b := range blocks {
		if b.Type == "tool_use" {
			step := describeTool(b.Name, b.Input)
			step.At = entry.Timestamp
			steps = append(steps, step)
		}
	}
	return steps
}

// describeTool turns a tool call into a step. A shell step shows the command
// itself, never the agent's description of it, which could say anything.
func describeTool(name string, raw json.RawMessage) ActivityStep {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Pattern      string `json:"pattern"`
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal(raw, &in)
	step := ActivityStep{Kind: "other", Text: name}
	switch name {
	case "Bash":
		step.Kind, step.Risk = "run", Risk(in.Command)
		step.Text = in.Command
	case "Read":
		step.Kind, step.Text = "read", in.FilePath
	case "Grep", "Glob":
		step.Kind, step.Text = "read", in.Pattern
	case "Edit", "MultiEdit", "Write":
		step.Kind, step.Text = "edit", in.FilePath
	case "NotebookEdit":
		step.Kind, step.Text = "edit", in.NotebookPath
	case "Agent", "Task":
		step.Kind, step.Text = "agent", strings.TrimSpace(in.SubagentType+": "+in.Description)
	}
	step.Text = cleanStepText(step.Text)
	return step
}

func cleanStepText(text string) string {
	clean, _ := redact.Redact(text)
	return github.Printable(strings.Join(strings.Fields(clean), " "), activityTextLimit)
}

// trimSteps keeps the newest limit steps and every risky step before them.
func trimSteps(steps []ActivityStep, limit int) []ActivityStep {
	if limit <= 0 || len(steps) <= limit {
		return steps
	}
	cut := len(steps) - limit
	kept := make([]ActivityStep, 0, limit)
	for _, s := range steps[:cut] {
		if s.Risk != "" && len(kept) < limit {
			kept = append(kept, s)
		}
	}
	return append(kept, steps[cut:]...)
}
