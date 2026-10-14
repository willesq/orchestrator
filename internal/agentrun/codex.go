package agentrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/models"
)

// ProviderCodex is the provider name that runs a task on Codex.
const ProviderCodex = models.ProviderCodex

// CodexOptions configures Codex runs.
type CodexOptions struct {
	Bin string
	// Sandbox is Codex's sandbox mode for the session's commands. Only the two
	// sandboxed modes are allowed: a queued session is unattended, and full
	// access would hand an untrusted repository the whole machine.
	Sandbox string
	// Network lets sandboxed commands reach the network, which pushing a branch
	// and opening a PR need.
	Network bool
	// StateDir keeps each task's Codex thread id, so a task interrupted by a
	// restart resumes its own session, and its scratch files, which must sit
	// outside every root the sandbox can write.
	StateDir string
	// MCPAllow names the MCP servers a session keeps; every other configured
	// server is turned off, since MCP servers run outside the sandbox.
	MCPAllow []string
	// OrchBin is the orch CLI. An allowlisted server is kept only if it runs
	// this binary, so a repository's config cannot reuse an allowed name for a
	// command of its own.
	OrchBin string
	// AllowProjectConfig runs tasks in checkouts that carry their own .codex
	// directory. Off, they are refused: project config can redirect the model
	// provider, which no command-line pin here covers.
	AllowProjectConfig bool
}

// codexSandboxes are the sandbox modes a headless Codex run may use.
var codexSandboxes = []string{"workspace-write", "read-only"}

// taskIDRe bounds the task id written into Codex's config overrides.
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func (c CodexOptions) check(t Task) error {
	if c.Bin == "" {
		return errors.New("no Codex CLI configured; set queue.worker.codex.bin and queue.worker.codex.enabled")
	}
	if !slices.Contains(codexSandboxes, c.Sandbox) {
		return fmt.Errorf("codex sandbox %q is not allowed for headless runs (use one of %s)",
			c.Sandbox, strings.Join(codexSandboxes, ", "))
	}
	if t.Effort != "" && !models.ValidEffort(t.Effort) {
		return fmt.Errorf("invalid effort %q", t.Effort)
	}
	if !taskIDRe.MatchString(t.ID) {
		return fmt.Errorf("invalid task id %q", t.ID)
	}
	if c.StateDir == "" {
		return errors.New("no Codex state directory configured")
	}
	return nil
}

func (r *Runner) codexThreadFile(id string) string {
	return filepath.Join(r.Codex.StateDir, id+".thread")
}

// codexThread returns the Codex thread an earlier run of task id started, or "".
func (r *Runner) codexThread(id string) string {
	if r.Codex.StateDir == "" || !taskIDRe.MatchString(id) {
		return ""
	}
	b, err := os.ReadFile(r.codexThreadFile(id))
	if err != nil {
		return ""
	}
	th := strings.TrimSpace(string(b))
	if !threadRe.MatchString(th) {
		return ""
	}
	return th
}

// threadRe is a Codex thread id; it becomes an argument, so it may not start
// with "-".
var threadRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

func (r *Runner) forgetCodexThread(id string) {
	if r.Codex.StateDir != "" && taskIDRe.MatchString(id) {
		_ = os.Remove(r.codexThreadFile(id))
	}
}

func (r *Runner) saveCodexThread(id, thread string) {
	if thread == "" || !threadRe.MatchString(thread) {
		return
	}
	if err := os.MkdirAll(r.Codex.StateDir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(r.codexThreadFile(id), []byte(thread+"\n"), 0o600)
}

// codexArgs builds the exec invocation. The prompt always comes from stdin
// ("-"), never argv. Every setting that widens what the session can do is
// pinned here, so neither user nor project config can loosen it for a queued run.
func (r *Runner) codexArgs(ctx context.Context, t Task, dir, lastMsg, thread string, env []string) ([]string, error) {
	disable, err := r.codexMCPToDisable(ctx, dir, env)
	if err != nil {
		return nil, err
	}
	common := []string{"--json", "--skip-git-repo-check", "--ignore-rules", "-o", lastMsg,
		// Nobody can answer an approval prompt; a command that would need one fails.
		"-c", `approval_policy="never"`,
		"-c", fmt.Sprintf("sandbox_mode=%q", r.Codex.Sandbox),
		"-c", fmt.Sprintf("sandbox_workspace_write.network_access=%t", r.Codex.Network),
		"-c", "sandbox_workspace_write.writable_roots=" + tomlStrings(r.codexWritableRoots(ctx, dir)),
		"-c", "notify=[]",
		"-c", "shell_environment_policy.ignore_default_excludes=false",
		// Codex filters what its shell commands inherit; set the task id
		// explicitly so `orch dispatch` from the session still refuses.
		"-c", fmt.Sprintf("shell_environment_policy.set.%s=%q", TaskIDEnv, t.ID),
	}
	for _, name := range disable {
		common = append(common, "-c", "mcp_servers."+name+".enabled=false")
	}
	if t.Model != "" {
		common = append(common, "-m", t.Model)
	}
	if t.Effort != "" {
		common = append(common, "-c", fmt.Sprintf("model_reasoning_effort=%q", t.Effort))
	}
	if thread != "" {
		return append(append([]string{"exec", "resume"}, common...), thread, "-"), nil
	}
	return append(append([]string{"exec"}, common...), "-C", dir, "-"), nil
}

// mcpName is a server name that is safe as a bare TOML key.
var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// codexMCPToDisable lists the MCP servers configured for dir and returns every
// enabled one outside the allowlist. MCP servers run outside the sandbox, and
// Codex starts them without the task's environment, so the queue server's own
// refusal cannot see the task: everything not explicitly allowed is turned off.
// It fails closed: a list it cannot read, or a name it cannot safely name,
// stops the run.
func (r *Runner) codexMCPToDisable(ctx context.Context, dir string, env []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Codex.Bin, "mcp", "list", "--json")
	cmd.Dir = dir
	cmd.Env = env
	out := &capped{max: 1 << 20}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list codex MCP servers: %w", err)
	}
	if out.over {
		return nil, errors.New("list codex MCP servers: output too large")
	}
	// Only names and commands are decoded: the listing also carries each
	// server's environment, which can hold credentials.
	var servers []struct {
		Name      string `json:"name"`
		Transport struct {
			Command string `json:"command"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(out.buf.Bytes(), &servers); err != nil {
		return nil, errors.New("list codex MCP servers: unreadable output")
	}
	var disable []string
	for _, sv := range servers {
		if slices.Contains(r.Codex.MCPAllow, sv.Name) && r.isOrch(sv.Transport.Command) {
			continue
		}
		if !mcpName.MatchString(sv.Name) {
			return nil, fmt.Errorf("codex MCP server %q cannot be turned off for this run", sv.Name)
		}
		// Every other server goes, whatever state it reports: turning off one
		// that is already off is harmless, and a missing field must not leave
		// one on.
		disable = append(disable, sv.Name)
	}
	return disable, nil
}

// isOrch reports whether command is the orch CLI.
func (r *Runner) isOrch(command string) bool {
	if r.Codex.OrchBin == "" || !filepath.IsAbs(command) {
		return false
	}
	a, errA := filepath.EvalSymlinks(command)
	b, errB := filepath.EvalSymlinks(r.Codex.OrchBin)
	return errA == nil && errB == nil && a == b
}

// capped is a writer that keeps at most max bytes and notes any excess.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// projectConfig returns a .codex directory between dir and the user's home,
// which Codex would load as project config, or "".
func projectConfig(dir, home string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if d == home || d == "/" || d == "." {
			return ""
		}
		if info, err := os.Stat(filepath.Join(d, ".codex")); err == nil && info.IsDir() {
			return filepath.Join(d, ".codex")
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// tomlStrings renders paths as a TOML array of basic strings.
func tomlStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// tomlString quotes s as a TOML basic string. Go's %q is not TOML: it emits
// escapes TOML lacks, and Codex reads a value that fails to parse as a raw
// literal instead of rejecting it.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexWritableRoots are the git paths a worktree session needs to commit,
// beyond its own directory: the shared object store, refs and logs, and the
// worktree's own git directory. Never the whole git directory, whose hooks and
// config would run unsandboxed the next time anyone uses git there. They are
// granted only when dir is the top of a linked worktree whose layout checks out
// both ways, so a planted .git file cannot point them at another repository. A
// plain clone gets none: Codex keeps its .git read-only.
func (r *Runner) codexWritableRoots(ctx context.Context, dir string) []string {
	top, gitDir, common := r.gitPaths(ctx, dir)
	if top == "" {
		return nil
	}
	real := func(p string) string {
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			return rp
		}
		return ""
	}
	topR, gitDirR, commonR := real(top), real(gitDir), real(common)
	dirR := real(dir)
	if topR == "" || gitDirR == "" || commonR == "" || dirR != topR {
		return nil
	}
	if filepath.Base(commonR) != ".git" || filepath.Dir(gitDirR) != filepath.Join(commonR, "worktrees") {
		return nil
	}
	back, err := os.ReadFile(filepath.Join(gitDirR, "gitdir"))
	if err != nil || real(strings.TrimSpace(string(back))) != real(filepath.Join(topR, ".git")) {
		return nil
	}
	roots := []string{gitDirR}
	for _, sub := range []string{"objects", "refs", "logs"} {
		p := filepath.Join(commonR, sub)
		if info, err := os.Lstat(p); err == nil && info.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// gitPaths returns dir's checkout top, git directory and shared git directory,
// or empty strings when dir is not in a repository.
func (r *Runner) gitPaths(ctx context.Context, dir string) (top, gitDir, common string) {
	out, err := r.git(ctx, dir, "rev-parse", "--path-format=absolute",
		"--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return "", "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		return "", "", ""
	}
	for _, l := range lines {
		if !filepath.IsAbs(l) {
			return "", "", ""
		}
	}
	return lines[0], lines[1], lines[2]
}

// codexSession runs one Codex invocation for the task.
func (r *Runner) codexSession(ctx context.Context, t Task, dir, prompt string, resume bool) (Outcome, error) {
	// Not under the system temp directory: the sandbox can write there, and
	// the session must not be able to rewrite its own recorded result.
	if err := os.MkdirAll(r.Codex.StateDir, 0o700); err != nil {
		return failed("codex scratch: %v", err), nil
	}
	tmp, err := os.MkdirTemp(r.Codex.StateDir, "run-*")
	if err != nil {
		return failed("codex scratch: %v", err), nil
	}
	defer os.RemoveAll(tmp)
	lastMsg := filepath.Join(tmp, "last-message.txt")
	thread := ""
	if resume {
		thread = r.codexThread(t.ID)
	}
	if pc := projectConfig(dir, r.Home); pc != "" && !r.Codex.AllowProjectConfig {
		return failed("refusing a Codex run under %s: project config can redirect the model provider "+
			"(set queue.worker.codex.allow_project_config to allow it)", pc), nil
	}
	env := append(sessionEnv(os.Environ(), r.searchPath(), r.DropEnv), TaskIDEnv+"="+t.ID)
	args, err := r.codexArgs(ctx, t, dir, lastMsg, thread, env)
	if err != nil {
		return failed("%v", err), nil
	}
	guard := r.newGitGuard(ctx, dir)
	run, err := r.runCLI(ctx, r.Codex.Bin, args, dir, prompt, env)
	if err != nil {
		return failed("%v", err), nil
	}
	// Checked on every exit, interrupted and timed-out runs included: the
	// tampering would otherwise outlive the session.
	if tampered := guard.restore(ctx); len(tampered) > 0 {
		return failed("the session changed git state outside its worktree; restored: %s",
			strings.Join(tampered, ", ")), nil
	}
	ev := parseCodexEvents(run.stdout)
	// Kept even when interrupted: that is exactly when the next run resumes it.
	r.saveCodexThread(t.ID, ev.thread)
	if run.interrupted(r) {
		return Outcome{}, interruptedErr(t)
	}
	var o Outcome
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o = failed("timed out after %s", r.Timeout)
	} else {
		last, _ := os.ReadFile(lastMsg)
		o = interpretCodex(ev, string(last), run.stderr, run.err)
	}
	o.Metrics.SessionID = ev.thread
	if t.Model != "" {
		o.Metrics.Models = []string{t.Model}
	}
	o.DurationMS = run.elapsedMS
	return o, nil
}

// maxEventLine bounds one JSONL event the parser decodes.
const maxEventLine = 4 << 20

// codexEvents is what the ledger needs from `codex exec --json`.
type codexEvents struct {
	thread    string
	turns     int
	lastText  string
	failure   string
	tokensIn  int64
	tokensOut int64
}

func parseCodexEvents(stdout []byte) codexEvents {
	var ev codexEvents
	// Lines are read whole and an oversized one is skipped, never fatal: a
	// Scanner would stop at the first long line and miss a later failure.
	rd := bufio.NewReader(bytes.NewReader(stdout))
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		if len(line) > maxEventLine {
			continue
		}
		var e struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "thread.started":
			ev.thread = e.ThreadID
		case "turn.completed":
			ev.turns++
			ev.tokensIn += e.Usage.Input
			ev.tokensOut += e.Usage.Output
		case "item.completed":
			if e.Item.Type == "agent_message" {
				ev.lastText = e.Item.Text
			}
		case "turn.failed":
			ev.failure = e.Error.Message
		case "error":
			ev.failure = e.Message
		}
	}
	return ev
}

func interpretCodex(ev codexEvents, last string, stderr []byte, runErr error) Outcome {
	text := strings.TrimSpace(last)
	if text == "" {
		text = strings.TrimSpace(ev.lastText)
	}
	var o Outcome
	switch {
	case runErr != nil || ev.failure != "":
		msg := ev.failure
		if msg == "" {
			msg = strings.TrimSpace(string(stderr))
		}
		if text != "" {
			msg = strings.TrimSpace(msg + "\n" + text)
		}
		if runErr != nil {
			msg = fmt.Sprintf("codex exited: %v: %s", runErr, msg)
		}
		o = outcome(StatusFailed, msg)
	case text == "":
		o = failed("codex finished without a final message")
	case strings.HasPrefix(text, "BLOCKED:"):
		o = outcome(StatusBlocked, text)
	default:
		o = outcome(StatusComplete, text)
	}
	// Codex reports tokens, not cost; cost stays zero rather than a guess.
	o.Metrics = metricsFrom(cliResult{NumTurns: ev.turns}, o.Result)
	return o
}
