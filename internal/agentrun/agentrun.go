// Package agentrun executes a dispatched queue task as a headless Claude Code
// session: `claude -p --agent <name>` in the task's working directory, with the
// task body on stdin and the final message captured as the result.
//
// Runs are resumable. Each task maps to a deterministic session ID, so when orchd
// stops mid-run and the workflow engine hands the task out again, the runner
// finds that session's transcript and continues it with --resume instead of
// starting the work over.
package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/redact"
)

// Task is one unit of dispatched work.
type Task struct {
	ID    string
	Agent string
	Body  string
	// Cwd is the absolute directory the session runs in; empty uses a private
	// scratch directory for the task.
	Cwd string
}

// Statuses a finished run records in the task ledger.
const (
	StatusComplete = "complete"
	StatusFailed   = "failed"
	StatusBlocked  = "blocked"
)

// Outcome is a finished run.
type Outcome struct {
	Status string
	Result string
}

// TaskIDEnv is set in the session's environment. The queue MCP server refuses
// to dispatch from inside a task, so a worker cannot fan out unboundedly.
const TaskIDEnv = "ORCH_TASK_ID"

// maxResultBytes caps the stored result; the ledger row is surfaced verbatim
// through queue_read and should stay readable.
const maxResultBytes = 64 << 10

// agentName is the shape of a Claude Code subagent name. Validating it keeps a
// dispatched value from being parsed as a CLI flag.
var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// allowedModes are the permission modes a headless run may use. Each decides
// without a human; bypassPermissions is excluded because an unattended session
// working in an arbitrary repository must keep the policy's guard rails.
var allowedModes = []string{"auto", "dontAsk", "acceptEdits", "plan"}

// ValidPermissionMode reports whether mode may be used for a headless run.
func ValidPermissionMode(mode string) bool { return slices.Contains(allowedModes, mode) }

// dispatchTools are denied to every run so a task cannot queue more tasks. This
// is a first line only; the daemon's cap on active tasks is the actual bound,
// since a session could reach orchd some other way.
const dispatchTools = "mcp__queue__queue_dispatch,Bash(orch dispatch:*)"

// ValidAgent reports whether name can be passed to --agent.
func ValidAgent(name string) bool { return agentName.MatchString(name) }

// sessionNamespace derives a task's session ID; changing it would orphan the
// sessions of in-flight tasks, which could then no longer be resumed.
var sessionNamespace = uuid.MustParse("6f0d3f52-6c3b-4c1e-9a4e-2d5b9f1c7a10")

// SessionID is the Claude Code session a task runs in.
func SessionID(taskID string) string {
	return uuid.NewSHA1(sessionNamespace, []byte(taskID)).String()
}

// Runner runs tasks through the Claude Code CLI.
type Runner struct {
	Bin string // absolute path to the claude binary
	// DropEnv names variables withheld from the session, such as the vector
	// store's API key that orchd itself holds.
	DropEnv []string
	// AllowedTools are permission rules granted to every run without asking
	// (queue.worker.allowed_tools.claude). They matter most under dontAsk, where
	// anything not allowed is refused. The dispatch tools stay denied regardless,
	// since Claude Code applies deny rules before allow rules.
	AllowedTools   []string
	PermissionMode string        // --permission-mode for every run
	Timeout        time.Duration // per-run cap; zero means none
	Home           string        // user home, for transcripts and PATH
	ScratchDir     string        // parent of per-task directories for Cwd-less tasks
	// Stop ends in-flight runs when canceled. go-workflows runs activities on a
	// background context so a stopping worker waits for them; without this an
	// orchd shutdown would block for as long as the longest task.
	Stop context.Context
}

// Run executes t and reports its outcome. A run the agent itself fails, or that
// cannot start, is a failed outcome with a nil error: agent work has side effects
// (branches, PRs), so it must never be retried blindly. A run interrupted by Stop
// returns an error wrapping context.Canceled, which go-workflows treats as
// abandoned; the task is handed out again after orchd restarts and resumes.
func (r *Runner) Run(ctx context.Context, t Task) (Outcome, error) {
	if !ValidAgent(t.Agent) {
		return failed("invalid agent name %q", t.Agent), nil
	}
	if r.Bin == "" {
		return failed("no Claude Code CLI found; set queue.worker.claude_bin"), nil
	}
	if !ValidPermissionMode(r.PermissionMode) {
		return failed("permission mode %q is not allowed for headless runs (use one of %s)",
			r.PermissionMode, strings.Join(allowedModes, ", ")), nil
	}
	dir, err := r.workDir(t)
	if err != nil {
		return failed("%v", err), nil
	}

	if r.Stop != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(r.Stop, cancel)()
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	sid := SessionID(t.ID)
	args := []string{"-p", "--output-format", "json", "--permission-mode", r.PermissionMode,
		"--disallowedTools", dispatchTools}
	if rules := allowedRules(r.AllowedTools); rules != "" {
		// One token: the CLI's variadic parsing would otherwise read a following
		// value that begins with "-" as a flag of its own.
		args = append(args, "--allowedTools="+rules)
	}
	args = append(args, "--agent", t.Agent)
	prompt := taskPrompt(t)
	if r.transcriptExists(sid) {
		args = append(args, "--resume", sid)
		prompt = resumePrompt(t)
	} else {
		args = append(args, "--session-id", sid)
	}

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Env = append(sessionEnv(os.Environ(), r.searchPath(), r.DropEnv), TaskIDEnv+"="+t.ID)
	// The prompt goes over stdin, never argv, so a body starting with "-" cannot
	// be read as a flag.
	cmd.Stdin = strings.NewReader(prompt)
	// Output goes to files, not pipes. A tool the session leaves running in the
	// background inherits these descriptors; with a pipe, Wait would block until
	// that tool exited and then report a successful run as failed.
	stdout, err := captureFile()
	if err != nil {
		return failed("capture output: %v", err), nil
	}
	defer stdout.Close()
	stderr, err := captureFile()
	if err != nil {
		return failed("capture output: %v", err), nil
	}
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group: cancellation must reach the tools the session spawned,
	// not just the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	runErr := cmd.Run()
	// Tools the session backgrounded can outlive a normal exit, and WaitDelay
	// escalates only against the CLI itself. The group's ID stays reserved while
	// any member lives, so this reaches only the session's own leftovers.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if runErr != nil && r.Stop != nil && r.Stop.Err() != nil {
		return Outcome{}, fmt.Errorf("task %s interrupted by worker shutdown: %w", t.ID, context.Canceled)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failed("timed out after %s", r.Timeout), nil
	}
	return interpret(readCapture(stdout), readCapture(stderr), runErr), nil
}

// cliResult is the subset of `claude -p --output-format json` the ledger uses.
type cliResult struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Subtype string `json:"subtype"`
}

func interpret(stdout, stderr []byte, runErr error) Outcome {
	var res cliResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &res); err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(stdout))
		}
		if runErr != nil {
			return failed("claude exited: %v: %s", runErr, msg)
		}
		return failed("unreadable claude output: %s", msg)
	}
	text := strings.TrimSpace(res.Result)
	switch {
	case res.IsError || runErr != nil:
		if text == "" {
			text = strings.TrimSpace(res.Subtype + " " + strings.TrimSpace(string(stderr)))
		}
		return outcome(StatusFailed, text)
	case strings.HasPrefix(text, "BLOCKED:"):
		return outcome(StatusBlocked, text)
	default:
		return outcome(StatusComplete, text)
	}
}

// maxCaptureBytes bounds how much CLI output is read back; the JSON result is
// far smaller, and a runaway stream must not exhaust orchd's memory.
const maxCaptureBytes = 8 << 20

// captureFile is an anonymous, owner-only file for one stream of CLI output. It
// is unlinked immediately, so nothing is left behind even if orchd dies.
func captureFile() (*os.File, error) {
	f, err := os.CreateTemp("", "orch-task-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	return f, nil
}

func readCapture(f *os.File) []byte {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, maxCaptureBytes))
	return b
}

func failed(format string, args ...any) Outcome {
	return outcome(StatusFailed, fmt.Sprintf(format, args...))
}

// outcome redacts and caps the result: it is stored in orch.db and replayed into
// later sessions' context through the digest and queue_read.
func outcome(status, text string) Outcome {
	text, _ = redact.Redact(text)
	if len(text) > maxResultBytes {
		cut := maxResultBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "\n…[truncated]"
	}
	return Outcome{Status: status, Result: text}
}

func (r *Runner) workDir(t Task) (string, error) {
	if t.Cwd == "" {
		dir := filepath.Join(r.ScratchDir, t.ID)
		return dir, os.MkdirAll(dir, 0o700)
	}
	if !filepath.IsAbs(t.Cwd) {
		return "", fmt.Errorf("working directory %q is not absolute", t.Cwd)
	}
	info, err := os.Stat(t.Cwd)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory %q is not a directory", t.Cwd)
	}
	return t.Cwd, nil
}

// transcriptExists reports whether Claude Code has persisted the session, which
// it stores per project directory as <home>/.claude/projects/<dir>/<id>.jsonl.
func (r *Runner) transcriptExists(sid string) bool {
	matches, _ := filepath.Glob(filepath.Join(r.Home, ".claude", "projects", "*", sid+".jsonl"))
	return len(matches) > 0
}

// searchPath is the PATH a session gets. orchd inherits launchd's minimal PATH,
// which finds neither Homebrew's tools nor the user's; the agent's own shell
// commands load the login profile, but the CLI and anything it execs directly
// resolve through this.
func (r *Runner) searchPath() string {
	dirs := []string{filepath.Dir(r.Bin), "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin"}
	if r.Home != "" {
		dirs = append(dirs, filepath.Join(r.Home, ".local", "bin"), filepath.Join(r.Home, "go", "bin"))
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// allowedRules joins the configured rules into one --allowedTools value. It
// drops blank rules, rules containing a comma (the CLI would split them into two
// different rules), and rules beginning with "-", which are never permission
// rules and could otherwise smuggle a flag such as one disabling permission
// checks.
func allowedRules(rules []string) string {
	var kept []string
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r != "" && !strings.Contains(r, ",") && !strings.HasPrefix(r, "-") {
			kept = append(kept, r)
		}
	}
	return strings.Join(kept, ",")
}

// sessionEnv is orchd's environment with PATH replaced and the task marker and
// the dropped variables removed.
func sessionEnv(env []string, path string, drop []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "PATH" || name == TaskIDEnv || slices.Contains(drop, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+path)
}

// FindClaude resolves the CLI: the configured path, else PATH, else the
// locations the installers use.
func FindClaude(configured, home string) string {
	if configured != "" {
		return configured
	}
	if p, err := exec.LookPath("claude"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	candidates := []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude"}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude"), filepath.Join(home, ".claude", "local", "claude"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}

func taskPrompt(t Task) string {
	return "You are running headless as orchestrator queue task " + t.ID + ". Nobody is watching this " +
		"session and nobody can answer questions: permission prompts are decided automatically, and " +
		"an action that is not allowed fails instead of asking.\n\n" +
		"Work only on the task below. Your final message is recorded verbatim as the task result, so " +
		"make it self-contained: what you did, links to anything you created (branches, commits, PRs), " +
		"and what is left. If you cannot proceed, make your final message start with \"BLOCKED:\" " +
		"followed by exactly what you need.\n\n## Task\n\n" + t.Body + "\n"
}

func resumePrompt(t Task) string {
	return "orchd restarted while you were working on queue task " + t.ID + ". Re-check the state of " +
		"your work (git status, branches, open PRs) and continue the task from where you left off. " +
		"The same rules apply: your final message is recorded as the task result, and it must start " +
		"with \"BLOCKED:\" if you cannot proceed.\n"
}
