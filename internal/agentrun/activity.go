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
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/redact"
)

// activityTail is how much of a transcript's end is read for its activity, so
// a long session costs the same each time it is polled.
const activityTail = 4 << 20

// Limits on what a session's activity carries, so a poll stays small however
// long the session gets.
const (
	activityTextLimit    = 240
	activityDetailLimit  = 2 << 10
	activityOutputLimit  = 3 << 10 / 2
	activityOutputSteps  = 100
	activityCommandLimit = 8
	activityCommandSize  = 160
	activityFileLimit    = 100
	activityToolLimit    = 80
)

// ActivityStep is one thing a session did.
type ActivityStep struct {
	Seq      int    // its place among all steps read
	At       string // RFC 3339, as the transcript recorded it
	Kind     string // read | edit | run | agent | other
	Tool     string
	Text     string   // the command, path or pattern on one line, redacted and capped
	Title    string   // its main command, or a path
	Detail   string   // the full command or pattern, keeping its lines
	Commands []string // the first few of its commands
	// CommandTotal counts them all.
	CommandTotal int
	Risk         string // what makes it risky, or ""

	// From the tool's result, once it has arrived.
	Finished   bool
	Failed     bool
	Output     string
	DurationMS int64

	Additions, Deletions int // lines an edit adds and removes
}

// ToolCount is how often a session used a tool.
type ToolCount struct {
	Name  string
	Count int
}

// FileEdit is a file a session edited, with its lines summed across edits.
type FileEdit struct {
	Path                 string
	Additions, Deletions int
}

// Activity is what a task's session has done so far. The counts cover every
// step read, including steps dropped from Steps.
type Activity struct {
	Available bool
	Steps     []ActivityStep
	Total     int

	Tools     []ToolCount
	Files     []FileEdit
	FilesRead int
	Commands  int
	Failed    int
	StartedAt string
	LastAt    string
	Pushed    bool // a step ran git push, listed or not
	Risky     int  // risky steps, listed or not
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
// The newest limit steps are kept, plus up to limit of the newest risky ones
// before them. Pushed and Risky count every step read.
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
	out := summarize(steps)
	out.Available = true
	out.Total = len(steps)
	out.Steps = trimSteps(steps, limit)
	return out, nil
}

// parseActivity reads a transcript's tool calls and matches each to its result.
// The transcript is being written while it is read, and past the tail's start
// the first line is partial, so lines that do not parse are skipped.
func parseActivity(r io.Reader, partialFirst bool) ([]ActivityStep, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var steps []ActivityStep
	open := map[string]int{}
	for first := true; ; first = false {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && (!first || !partialFirst) &&
			(bytes.Contains(line, []byte(`"tool_use"`)) || bytes.Contains(line, []byte(`"tool_result"`))) {
			steps = applyActivityLine(line, steps, open)
		}
		if err == io.EOF {
			return steps, nil
		}
		if err != nil {
			return steps, err
		}
	}
}

func applyActivityLine(line []byte, steps []ActivityStep, open map[string]int) []ActivityStep {
	var entry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || (entry.Type != "assistant" && entry.Type != "user") {
		return steps
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return steps
	}
	for _, b := range blocks {
		switch {
		case b.Type == "tool_use" && entry.Type == "assistant":
			step := describeTool(b.Name, b.Input)
			// Only a real time: it goes to the app with every step, uncapped.
			if _, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				step.At = entry.Timestamp
			}
			step.Seq = len(steps)
			if b.ID != "" {
				open[b.ID] = len(steps)
			}
			steps = append(steps, step)
		case b.Type == "tool_result" && entry.Type == "user":
			index, ok := open[b.ToolUseID]
			if !ok {
				continue
			}
			delete(open, b.ToolUseID)
			step := &steps[index]
			step.Finished, step.Failed = true, b.IsError
			step.Output = cleanOutput(resultText(b.Content))
			step.DurationMS = elapsedMS(step.At, entry.Timestamp)
		}
	}
	return steps
}

// resultText reads a tool result's content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func elapsedMS(start, end string) int64 {
	from, err1 := time.Parse(time.RFC3339Nano, start)
	to, err2 := time.Parse(time.RFC3339Nano, end)
	if err1 != nil || err2 != nil || to.Before(from) {
		return 0
	}
	return to.Sub(from).Milliseconds()
}

// describeTool turns a tool call into a step. A shell step shows the command
// itself, never the agent's description of it, which could say anything.
func describeTool(name string, raw json.RawMessage) ActivityStep {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
		OldString    string `json:"old_string"`
		NewString    string `json:"new_string"`
		Content      string `json:"content"`
		Edits        []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	_ = json.Unmarshal(raw, &in)
	label := oneLine(name, activityToolLimit)
	step := ActivityStep{Kind: "other", Tool: label, Text: label}
	switch name {
	case "Bash":
		step.Kind, step.Risk = "run", Risk(in.Command)
		step.Text = in.Command
		step.Detail = in.Command
		// Redact before splitting: a secret split across commands no longer
		// looks like one.
		commands := splitCommands(redactClean(in.Command))
		step.CommandTotal = len(commands)
		for _, command := range commands[:min(len(commands), activityCommandLimit)] {
			step.Commands = append(step.Commands, oneLine(command, activityCommandSize))
		}
		step.Title = mainCommand(step.Commands)
	case "Read":
		step.Kind, step.Text = "read", in.FilePath
	case "Grep", "Glob":
		step.Kind, step.Text = "read", in.Pattern
		if in.Path != "" {
			step.Detail = in.Pattern + "\nin " + in.Path
		}
	case "Edit":
		step.Kind, step.Text = "edit", in.FilePath
		step.Additions, step.Deletions = lineCount(in.NewString), lineCount(in.OldString)
	case "MultiEdit":
		step.Kind, step.Text = "edit", in.FilePath
		for _, e := range in.Edits {
			step.Additions += lineCount(e.NewString)
			step.Deletions += lineCount(e.OldString)
		}
	case "Write":
		step.Kind, step.Text = "edit", in.FilePath
		step.Additions = lineCount(in.Content)
	case "NotebookEdit":
		step.Kind, step.Text = "edit", in.NotebookPath
	case "Agent", "Task":
		step.Kind, step.Text = "agent", strings.TrimSpace(in.SubagentType+": "+in.Description)
	}
	step.Text = cleanStepText(step.Text)
	if step.Title == "" {
		step.Title = step.Text
	}
	step.Detail = cleanMultiline(step.Detail, activityDetailLimit, false)
	return step
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

func cleanStepText(text string) string { return oneLine(text, activityTextLimit) }

func oneLine(text string, limit int) string {
	return github.Printable(strings.Join(strings.Fields(redactClean(text)), " "), limit)
}

// redactClean drops control characters and then redacts: the other way round, a
// control character inside a secret hides it from redaction, and removing the
// character afterwards puts the secret back together.
func redactClean(text string) string {
	clean, _ := redact.Redact(strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "")))
	return clean
}

// cleanOutput keeps the end of what a tool returned, where errors are.
func cleanOutput(text string) string {
	return cleanMultiline(text, activityOutputLimit, true)
}

// cleanMultiline redacts text and drops control characters line by line, keeping
// its start or its end within limit bytes.
func cleanMultiline(text string, limit int, keepEnd bool) string {
	clean := redactClean(text)
	if keepEnd && len(clean) > limit {
		clean = clean[len(clean)-limit:]
		// Start at a line, and say that something came before it.
		if i := strings.IndexByte(clean, '\n'); i >= 0 && i < len(clean)-1 {
			clean = clean[i+1:]
		}
		clean = "…\n" + clean
	} else if len(clean) > limit {
		clean = clean[:limit]
	}
	return strings.TrimRight(strings.ToValidUTF8(clean, ""), " \t\r\n")
}

// splitCommands splits a shell line into its commands at ;, &&, || and newlines
// outside quotes, parentheses and here-documents. A pipeline stays one command.
// It only reads for display, so a shape it misjudges shows oddly and nothing else.
func splitCommands(command string) []string {
	var out []string
	var current strings.Builder
	flush := func() {
		text := strings.TrimSpace(current.String())
		current.Reset()
		if text != "" && !strings.HasPrefix(text, "#") {
			out = append(out, text)
		}
	}
	var quote byte
	depth := 0
	heredoc := ""
	for i := 0; i < len(command); i++ {
		c := command[i]
		if heredoc != "" {
			end := strings.IndexByte(command[i:], '\n')
			if end < 0 {
				current.WriteString(" …")
				break
			}
			line := command[i : i+end]
			i += end
			if strings.TrimSpace(line) == heredoc {
				heredoc = ""
				current.WriteString(" …")
				flush()
			}
			continue
		}
		switch {
		case quote != 0:
			current.WriteByte(c)
			if c == '\\' && quote == '"' && i+1 < len(command) {
				i++
				current.WriteByte(command[i])
			} else if c == quote {
				quote = 0
			}
		case c == '\\' && i+1 < len(command):
			if command[i+1] == '\n' {
				current.WriteByte(' ')
			} else {
				current.WriteByte(c)
				current.WriteByte(command[i+1])
			}
			i++
		case c == '\'' || c == '"':
			quote = c
			current.WriteByte(c)
		case c == '(':
			depth++
			current.WriteByte(c)
		case c == ')':
			depth = max(0, depth-1)
			current.WriteByte(c)
		case depth > 0:
			current.WriteByte(c)
		case strings.HasPrefix(command[i:], "<<<"):
			current.WriteString("<<<")
			i += 2
		case c == '<' && strings.HasPrefix(command[i:], "<<"):
			if m := heredocStart.FindStringSubmatch(command[i:]); m != nil {
				current.WriteString(strings.TrimRight(m[0], " \t"))
				heredoc = m[1]
				i += len(m[0]) - 1
				rest := strings.IndexByte(command[i+1:], '\n')
				if rest < 0 {
					i = len(command)
				} else {
					current.WriteString(command[i+1 : i+1+rest])
					i += rest + 1
				}
			} else {
				current.WriteByte(c)
			}
		case c == ';' || c == '\n':
			flush()
		case (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c:
			flush()
			i++
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return out
}

var (
	heredocStart = regexp.MustCompile(`^<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	assignment   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*$`)
)

// mainCommand picks the command that says what a step does, skipping moves and
// output that only set the scene for it.
func mainCommand(commands []string) string {
	for _, command := range commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "cd", "pwd", "echo", "printf", "export", "set", "sleep", "true", ":":
			continue
		}
		if len(fields) == 1 && assignment.MatchString(fields[0]) {
			continue
		}
		return command
	}
	if len(commands) > 0 {
		return commands[0]
	}
	return ""
}

// summarize counts what steps did. Paths are compared as the agent wrote them.
func summarize(steps []ActivityStep) Activity {
	var out Activity
	tools := map[string]int{}
	read := map[string]bool{}
	edits := map[string]int{}
	for _, step := range steps {
		tools[step.Tool]++
		if step.Risk != "" {
			out.Risky++
		}
		if step.Risk == "git push" || step.Risk == "force push" {
			out.Pushed = true
		}
		if step.Failed {
			out.Failed++
		}
		if out.StartedAt == "" {
			out.StartedAt = step.At
		}
		if step.At != "" {
			out.LastAt = step.At
		}
		switch step.Kind {
		case "run":
			out.Commands += max(1, step.CommandTotal)
		case "read":
			if step.Tool == "Read" {
				read[step.Text] = true
			}
		case "edit":
			index, ok := edits[step.Text]
			if !ok {
				if len(out.Files) == activityFileLimit {
					continue
				}
				index = len(out.Files)
				edits[step.Text] = index
				out.Files = append(out.Files, FileEdit{Path: step.Text})
			}
			out.Files[index].Additions += step.Additions
			out.Files[index].Deletions += step.Deletions
		}
	}
	out.FilesRead = len(read)
	for name, count := range tools {
		out.Tools = append(out.Tools, ToolCount{Name: name, Count: count})
	}
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Count != out.Tools[j].Count {
			return out.Tools[i].Count > out.Tools[j].Count
		}
		return out.Tools[i].Name < out.Tools[j].Name
	})
	return out
}

// trimSteps keeps the newest limit steps and the newest limit risky ones before
// them, and
// the output of only the newest few, which is what bounds a poll's size.
func trimSteps(steps []ActivityStep, limit int) []ActivityStep {
	kept := steps
	if limit > 0 && len(steps) > limit {
		cut := len(steps) - limit
		kept = make([]ActivityStep, 0, limit)
		for _, s := range steps[:cut] {
			if s.Risk != "" {
				kept = append(kept, s)
			}
		}
		// The newest risky steps, so an early flood cannot hide a later push.
		kept = append(kept[max(0, len(kept)-limit):], steps[cut:]...)
	}
	for i := 0; i < len(kept)-activityOutputSteps; i++ {
		// A failure's output is why anyone would open the step.
		if !kept[i].Failed {
			kept[i].Output = ""
		}
	}
	return kept
}
