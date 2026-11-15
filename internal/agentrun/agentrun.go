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
	"log"
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

	"github.com/willesq/orchestrator/internal/models"
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
	// Heads is the HEAD of each checkout of Cwd's repository when the task was
	// dispatched (see Snapshot). Verification tests only checkouts that moved
	// since; nil means no baseline, and verification is skipped.
	Heads map[string]string
	// Provider picks the CLI: "codex" runs Codex, anything else Claude Code.
	Provider string
	// Model is passed as the CLI's model flag; empty leaves its own setting.
	Model string
	// Effort is the reasoning effort, for providers that take one (Codex).
	Effort string
	// Space is the space whose memory the task reads, recorded at dispatch
	// (D-46); "" records none.
	Space string
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
	Metrics
	// Verification records the check run against the work: "passed: …",
	// "failed: …" or "skipped: …". Empty when verification is off or the run did
	// not complete.
	Verification string
}

// Metrics describe what a run cost and produced. Fields the CLI did not report
// (a crash before any output, say) stay zero, except the duration, which the
// runner measures itself.
type Metrics struct {
	CostUSD    float64
	DurationMS int64
	Turns      int
	SessionID  string
	Models     []string // models that did the work, sorted
	PRs        []string // pull request URLs the result mentions, in order
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
// is a first line only: orchd refuses Dispatch from processes it started, and
// its cap on active tasks bounds what slips past both.
const dispatchTools = "mcp__queue__queue_dispatch,Bash(orch dispatch:*)"

// readOnlyAgents may read GitHub but never change it: orchd alone posts what
// they conclude, and only what the user opted in to. Denying the writing
// commands enforces in the runner what their instructions already say.
var readOnlyAgents = map[string]bool{"pr-verdict": true}

// gitHubWriteTools are the commands readOnlyAgents are denied: every gh
// command that changes GitHub, gh api whole (a pattern cannot tell its reads
// from its writes), the token itself, raw HTTP clients, pushes, and orch's own
// database, which holds what orchd may post. Patterns match what the agent
// types, not what programs it starts do, so they are a guard rather than a
// sandbox.
const gitHubWriteTools = "Bash(gh pr review:*),Bash(gh pr comment:*),Bash(gh pr merge:*),Bash(gh pr close:*)," +
	"Bash(gh pr edit:*),Bash(gh pr ready:*),Bash(gh pr reopen:*),Bash(gh pr create:*),Bash(gh pr lock:*)," +
	"Bash(gh issue:*),Bash(gh api:*),Bash(gh auth:*),Bash(gh release:*),Bash(gh workflow:*),Bash(gh run:*)," +
	"Bash(gh repo edit:*),Bash(gh repo delete:*),Bash(gh secret:*),Bash(gh variable:*),Bash(gh label:*)," +
	"Bash(gh gist:*),Bash(git push:*),Bash(git -C:*),Bash(curl:*),Bash(wget:*),Bash(sqlite3:*),WebFetch"

// operatorTools are the commands that change orch's own settings, denied to every
// run so a task cannot widen its own limits or permissions.
const operatorTools = "Bash(orch config reset:*),Bash(orch docs add:*),Bash(orch docs remove:*)," +
	"Bash(orch docs enable:*),Bash(orch docs disable:*),Bash(orch docs repos:*),Bash(orch docs purge:*)"

// deniedTools lists what agent may not use in a run.
func deniedTools(agent string) string {
	denied := dispatchTools + "," + operatorTools
	if readOnlyAgents[agent] {
		return denied + "," + gitHubWriteTools
	}
	return denied
}

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
	AllowedTools []string
	// Verify configures the check run against the agent's work before a task
	// counts as complete.
	Verify         VerifyOptions
	PermissionMode string        // --permission-mode for every run
	Timeout        time.Duration // per-run cap; zero means none
	Home           string        // user home, for transcripts and PATH
	ScratchDir     string        // parent of per-task directories for Cwd-less tasks
	// Stop ends in-flight runs when canceled. go-workflows runs activities on a
	// background context so a stopping worker waits for them; without this an
	// orchd shutdown would block for as long as the longest task.
	Stop context.Context
	// Codex configures Codex runs; with no Bin, tasks routed to Codex fail.
	Codex CodexOptions
	// OnLive, when set, is told the task ID whenever a run starts, its
	// process starts or stops, or the run ends, so watchers see liveness
	// change without polling. Set it before Run is called.
	OnLive func(taskID string)

	live liveRuns
}

func (r *Runner) liveChanged(taskID string) {
	if r.OnLive != nil {
		r.OnLive(taskID)
	}
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
	// Re-checked here because the value becomes an argument, and the durable
	// input may predate the registry's validation.
	if t.Model != "" && !models.ValidSlug(t.Model) {
		return failed("invalid model %q", t.Model), nil
	}
	if t.Provider == ProviderCodex {
		if err := r.Codex.check(t); err != nil {
			return failed("%v", err), nil
		}
	} else {
		if r.Bin == "" {
			return failed("no Claude Code CLI found; set queue.worker.claude_bin"), nil
		}
		if !ValidPermissionMode(r.PermissionMode) {
			return failed("permission mode %q is not allowed for headless runs (use one of %s)",
				r.PermissionMode, strings.Join(allowedModes, ", ")), nil
		}
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

	// Each run gets its own cancel, so `orch task --cancel` stops this task
	// alone; Live exposes it for liveness.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	sid := SessionID(t.ID)
	if t.Provider == ProviderCodex {
		sid = r.codexThread(t.ID)
	}
	entry := r.live.track(LiveRun{TaskID: t.ID, Provider: t.Provider, Model: t.Model, SessionID: sid,
		Dir: dir, Space: t.Space, Started: time.Now()}, cancelRun)
	if entry == nil {
		return failed("task %s already has a session running in this worker; not starting a second", t.ID), nil
	}
	r.liveChanged(t.ID)
	defer func() {
		r.live.untrack(entry)
		r.liveChanged(t.ID)
	}()
	ctx = withPIDReport(ctx, func(pid int) {
		r.live.setPID(t.ID, pid)
		r.liveChanged(t.ID)
	})

	resume := r.canResume(t)
	prompt := taskPrompt(t)
	if resume {
		prompt = resumePrompt(t)
	}
	o, err := r.session(ctx, t, dir, prompt, resume)
	if err == nil && o.Status == StatusComplete && r.Verify.Enabled {
		o, err = r.verifyAndFix(ctx, t, dir, o)
	}
	if cancelled, reason := r.live.wasCancelled(entry); cancelled {
		// Reason: a cancelled run's session exited on a signal, which would
		// otherwise read as a failure or, if the worker was stopping too, an
		// interruption to resume.
		o, err = Outcome{Status: StatusCancelled, Result: reason, Metrics: o.Metrics}, nil
		// Cancelled is final, so nothing resumes in the scratch directory; the
		// session has exited, so what it cloned or downloaded there can go.
		r.removeScratch(t)
	}
	// A finished Codex task has nothing left to resume; an interrupted one
	// (err set) keeps its thread for the next run.
	if err == nil && t.Provider == ProviderCodex {
		r.forgetCodexThread(t.ID)
	}
	return o, err
}

// session runs one agent invocation for the task, on the CLI its provider
// names: a fresh session, or a resumed one carrying prompt as the next turn.
func (r *Runner) session(ctx context.Context, t Task, dir, prompt string, resume bool) (Outcome, error) {
	if t.Provider == ProviderCodex {
		return r.codexSession(ctx, t, dir, prompt, resume)
	}
	return r.claudeSession(ctx, t, dir, prompt, resume)
}

// canResume reports whether an earlier run of t left a session to continue.
func (r *Runner) canResume(t Task) bool {
	if t.Provider == ProviderCodex {
		return r.codexThread(t.ID) != ""
	}
	return r.transcriptExists(SessionID(t.ID))
}

// claudeSession runs one Claude Code invocation for the task.
func (r *Runner) claudeSession(ctx context.Context, t Task, dir, prompt string, resume bool) (Outcome, error) {
	sid := SessionID(t.ID)
	args := []string{"-p", "--output-format", "json", "--permission-mode", r.PermissionMode,
		"--disallowedTools", deniedTools(t.Agent)}
	if rules := allowedRules(r.AllowedTools); rules != "" {
		// One token: the CLI's variadic parsing would otherwise read a following
		// value that begins with "-" as a flag of its own.
		args = append(args, "--allowedTools="+rules)
	}
	args = append(args, "--agent", t.Agent)
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if resume {
		args = append(args, "--resume", sid)
	} else {
		args = append(args, "--session-id", sid)
	}

	env := append(sessionEnv(os.Environ(), r.searchPath(), r.DropEnv), TaskIDEnv+"="+t.ID)
	run, err := r.runCLI(ctx, r.Bin, args, dir, prompt, env)
	if err != nil {
		return failed("%v", err), nil
	}
	if run.interrupted(r) {
		return Outcome{}, interruptedErr(t)
	}
	var o Outcome
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o = failed("timed out after %s", r.Timeout)
	} else {
		o = interpret(run.stdout, run.stderr, run.err)
	}
	o.DurationMS = run.elapsedMS
	return o, nil
}

// cliRun is what one CLI invocation left behind.
type cliRun struct {
	stdout, stderr []byte
	err            error
	elapsedMS      int64
}

// interrupted reports whether the run ended because the worker is stopping.
func (c cliRun) interrupted(r *Runner) bool {
	return c.err != nil && r.Stop != nil && r.Stop.Err() != nil
}

func interruptedErr(t Task) error {
	return fmt.Errorf("task %s interrupted by worker shutdown: %w", t.ID, context.Canceled)
}

// runCLI runs one agent CLI invocation in its own process group and captures its
// output. The error is set only when the run could not be set up.
func (r *Runner) runCLI(ctx context.Context, bin string, args []string, dir, prompt string, env []string) (cliRun, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	// The prompt goes over stdin, never argv, so a body starting with "-" cannot
	// be read as a flag.
	cmd.Stdin = strings.NewReader(prompt)
	// Output goes to files, not pipes. A tool the session leaves running in the
	// background inherits these descriptors; with a pipe, Wait would block until
	// that tool exited and then report a successful run as failed.
	stdout, err := captureFile()
	if err != nil {
		return cliRun{}, fmt.Errorf("capture output: %w", err)
	}
	defer stdout.Close()
	stderr, err := captureFile()
	if err != nil {
		return cliRun{}, fmt.Errorf("capture output: %w", err)
	}
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group: cancellation must reach the tools the session spawned,
	// not just the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	started := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		reportPID(ctx, cmd.Process.Pid)
		runErr = cmd.Wait()
		// Reason: a PID outlives its process; shown after exit it could
		// name an unrelated process the user then signals.
		reportPID(ctx, 0)
	}
	elapsed := time.Since(started).Milliseconds()
	// Tools the session backgrounded can outlive a normal exit, and WaitDelay
	// escalates only against the CLI itself. The group's ID stays reserved while
	// any member lives, so this reaches only the session's own leftovers.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cliRun{stdout: readCapture(stdout), stderr: readCapture(stderr), err: runErr, elapsedMS: elapsed}, nil
}

// cliResult is the subset of `claude -p --output-format json` the ledger uses.
type cliResult struct {
	Result     string                     `json:"result"`
	IsError    bool                       `json:"is_error"`
	Subtype    string                     `json:"subtype"`
	CostUSD    float64                    `json:"total_cost_usd"`
	NumTurns   int                        `json:"num_turns"`
	SessionID  string                     `json:"session_id"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// prURL matches GitHub pull request links in an agent's final message.
var prURL = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`)

// metricsFrom extracts the run's metrics from the CLI result and the final text.
func metricsFrom(res cliResult, text string) Metrics {
	m := Metrics{CostUSD: res.CostUSD, Turns: res.NumTurns, SessionID: res.SessionID}
	for model := range res.ModelUsage {
		m.Models = append(m.Models, model)
	}
	slices.Sort(m.Models)
	for _, u := range prURL.FindAllString(text, -1) {
		if !slices.Contains(m.PRs, u) {
			m.PRs = append(m.PRs, u)
		}
	}
	return m
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
	var o Outcome
	switch {
	case res.IsError || runErr != nil:
		if text == "" {
			text = strings.TrimSpace(res.Subtype + " " + strings.TrimSpace(string(stderr)))
		}
		o = outcome(StatusFailed, text)
	case strings.HasPrefix(text, "BLOCKED:"):
		o = outcome(StatusBlocked, text)
	default:
		o = outcome(StatusComplete, text)
	}
	o.Metrics = metricsFrom(res, o.Result)
	return o
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

// removeScratch deletes the private scratch directory workDir made for t. A
// task with its own working directory is the user's checkout and is left
// alone, as is any path that would not sit directly under ScratchDir.
func (r *Runner) removeScratch(t Task) {
	if t.Cwd != "" || r.ScratchDir == "" {
		return
	}
	root := filepath.Clean(r.ScratchDir)
	dir := filepath.Join(root, t.ID)
	if filepath.Dir(dir) != root || filepath.Base(dir) != t.ID {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("agentrun: remove the scratch directory of cancelled task %s: %v", t.ID, err)
	}
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
	var dirs []string
	// Only real binaries: filepath.Dir("") is ".", which would put the
	// session's working directory, an untrusted checkout, on its PATH.
	for _, bin := range []string{r.Bin, r.Codex.Bin} {
		if filepath.IsAbs(bin) {
			dirs = append(dirs, filepath.Dir(bin))
		}
	}
	dirs = append(dirs, "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin")
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
	return findCLI("claude", configured, home, []string{".local/bin/claude", ".claude/local/claude"})
}

// FindCodex resolves the Codex CLI the same way FindClaude resolves Claude Code.
func FindCodex(configured, home string) string {
	return findCLI("codex", configured, home, []string{".local/bin/codex"})
}

// findCLI resolves a CLI: the configured path, then PATH, then the usual
// install locations, inHome relative to the user's home.
func findCLI(name, configured, home string, inHome []string) string {
	if configured != "" {
		return configured
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	candidates := []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name}
	if home != "" {
		for _, rel := range inHome {
			candidates = append(candidates, filepath.Join(home, rel))
		}
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
