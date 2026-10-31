package agentrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeClaude is a stand-in CLI that records how it was invoked into $FAKE_DIR
// and replays $FAKE_DIR/stdout, $FAKE_DIR/stderr and $FAKE_EXIT.
//
// The background child starts before anything is recorded: tests treat the
// args file as "the CLI is running" and may cancel the moment it appears, so
// the child must already exist by then for its fate to be observable.
const fakeClaude = `#!/bin/bash
d="$FAKE_DIR"
if [ -n "$FAKE_CHILD" ]; then sleep 30 & echo $! > "$d/child.pid"; fi
printf '%s\n' "$@" > "$d/args"
cat > "$d/stdin"
pwd -P > "$d/cwd"
env > "$d/env"
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
if [ -n "$FAKE_ON_RESUME" ] && printf '%s\n' "$@" | grep -qx -- --resume; then eval "$FAKE_ON_RESUME"; fi
[ -f "$d/stdout" ] && cat "$d/stdout"
[ -f "$d/stderr" ] && cat "$d/stderr" >&2
exit "${FAKE_EXIT:-0}"
`

type fixture struct {
	dir    string // FAKE_DIR: what the fake recorded
	home   string
	runner *Runner
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir, home := t.TempDir(), t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	for _, k := range []string{"FAKE_EXIT", "FAKE_SLEEP", "FAKE_CHILD", "FAKE_ON_RESUME", TaskIDEnv} {
		t.Setenv(k, "")
	}
	return fixture{dir: dir, home: home, runner: &Runner{
		Bin: bin, PermissionMode: "auto", Home: home, ScratchDir: filepath.Join(home, "tasks"),
	}}
}

func (f fixture) reply(t *testing.T, stdout string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "stdout"), []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) recorded(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("fake did not record %s: %v", name, err)
	}
	return string(b)
}

func (f fixture) ran() bool {
	_, err := os.Stat(filepath.Join(f.dir, "args"))
	return err == nil
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunInvokesClaudeHeadless(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"type":"result","subtype":"success","is_error":false,"result":"opened PR #7"}`)
	t.Setenv(TaskIDEnv, "inherited-parent") // must be replaced, not duplicated
	t.Setenv("QDRANT_API_KEY", "orchd-only-secret")
	f.runner.DropEnv = []string{"QDRANT_API_KEY"}
	cwd := t.TempDir()
	task := Task{ID: "task-1", Agent: "dev", Body: "-x fix the flaky test", Cwd: cwd}

	got, err := f.runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != StatusComplete || got.Result != "opened PR #7" {
		t.Errorf("outcome = %+v", got)
	}
	args := strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n")
	want := []string{"-p", "--output-format", "json", "--permission-mode", "auto",
		"--disallowedTools", "mcp__queue__queue_dispatch,Bash(orch dispatch:*)", "--agent", "dev", "--session-id", SessionID("task-1")}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
	stdin := f.recorded(t, "stdin")
	if !strings.Contains(stdin, "-x fix the flaky test") || !strings.Contains(stdin, "queue task task-1") {
		t.Errorf("prompt missing body or task id:\n%s", stdin)
	}
	if slices.Contains(args, "-x fix the flaky test") {
		t.Errorf("body leaked into argv")
	}
	if got := strings.TrimSpace(f.recorded(t, "cwd")); got != realpath(t, cwd) {
		t.Errorf("ran in %q, want %q", got, cwd)
	}
	env := f.recorded(t, "env")
	if strings.Count(env, TaskIDEnv+"=") != 1 || !strings.Contains(env, TaskIDEnv+"=task-1\n") {
		t.Errorf("env must carry exactly %s=task-1", TaskIDEnv)
	}
	if strings.Contains(env, "orchd-only-secret") {
		t.Errorf("session inherited orchd's vector store key")
	}
	if !strings.Contains(env, "PATH="+filepath.Dir(f.runner.Bin)+":/opt/homebrew/bin") {
		t.Errorf("PATH does not lead with the CLI dir and Homebrew")
	}
}

func TestRunResumesAnInterruptedSession(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"finished","is_error":false}`)
	sid := SessionID("task-2")
	transcript := filepath.Join(f.home, ".claude", "projects", "-src-widget", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.runner.Run(context.Background(), Task{ID: "task-2", Agent: "dev", Body: "b", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := f.recorded(t, "args")
	if !strings.Contains(args, "--resume\n"+sid+"\n") || strings.Contains(args, "--session-id") {
		t.Errorf("expected --resume %s, got:\n%s", sid, args)
	}
	if !strings.Contains(f.recorded(t, "stdin"), "orchd restarted") {
		t.Errorf("resume prompt not sent")
	}
}

func TestRunInterpretsOutcomes(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		exit       string
		wantStatus string
		wantIn     string
		wantNotIn  string
	}{
		{name: "agent error", stdout: `{"result":"could not build","is_error":true}`, wantStatus: StatusFailed, wantIn: "could not build"},
		{name: "error without text falls back to subtype", stdout: `{"result":"","is_error":true,"subtype":"error_max_turns"}`, exit: "1", wantStatus: StatusFailed, wantIn: "error_max_turns"},
		{name: "blocked", stdout: `{"result":"BLOCKED: need repo access","is_error":false}`, wantStatus: StatusBlocked, wantIn: "need repo access"},
		{name: "crash before any output", stderr: "auth expired", exit: "2", wantStatus: StatusFailed, wantIn: "auth expired"},
		{name: "unparseable success", stdout: "not json", wantStatus: StatusFailed, wantIn: "unreadable claude output"},
		{name: "secrets are redacted", stdout: `{"result":"token ` + secret + `","is_error":false}`, wantStatus: StatusComplete, wantIn: "[REDACTED:github-token]", wantNotIn: secret},
		{name: "huge results are capped", stdout: `{"result":"` + strings.Repeat("x", maxResultBytes+100) + `"}`, wantStatus: StatusComplete, wantIn: "[truncated]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.reply(t, tt.stdout)
			if tt.stderr != "" {
				if err := os.WriteFile(filepath.Join(f.dir, "stderr"), []byte(tt.stderr), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FAKE_EXIT", tt.exit)
			got, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Status != tt.wantStatus || !strings.Contains(got.Result, tt.wantIn) {
				t.Errorf("outcome = %s %q, want %s containing %q", got.Status, got.Result, tt.wantStatus, tt.wantIn)
			}
			if tt.wantNotIn != "" && strings.Contains(got.Result, tt.wantNotIn) {
				t.Errorf("result leaks %q", tt.wantNotIn)
			}
			if len(got.Result) > maxResultBytes+64 {
				t.Errorf("result not capped: %d bytes", len(got.Result))
			}
		})
	}
}

func TestRunRejectsBeforeSpawning(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(r *Runner)
		task   Task
		wantIn string
	}{
		{name: "agent that would read as a flag", task: Task{ID: "t", Agent: "--dangerously-skip-permissions", Body: "b"}, wantIn: "invalid agent"},
		{name: "no CLI resolved", mutate: func(r *Runner) { r.Bin = "" }, task: Task{ID: "t", Agent: "dev", Body: "b"}, wantIn: "claude_bin"},
		{name: "bypassPermissions is refused", mutate: func(r *Runner) { r.PermissionMode = "bypassPermissions" }, task: Task{ID: "t", Agent: "dev", Body: "b"}, wantIn: "not allowed"},
		{name: "empty permission mode is refused", mutate: func(r *Runner) { r.PermissionMode = "" }, task: Task{ID: "t", Agent: "dev", Body: "b"}, wantIn: "not allowed"},
		{name: "relative cwd", task: Task{ID: "t", Agent: "dev", Body: "b", Cwd: "src"}, wantIn: "not absolute"},
		{name: "missing cwd", task: Task{ID: "t", Agent: "dev", Body: "b", Cwd: "/nonexistent/orch-test"}, wantIn: "working directory"},
		{name: "cwd is a file", task: Task{ID: "t", Agent: "dev", Body: "b", Cwd: file}, wantIn: "not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.mutate != nil {
				tt.mutate(f.runner)
			}
			got, err := f.runner.Run(context.Background(), tt.task)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Status != StatusFailed || !strings.Contains(got.Result, tt.wantIn) {
				t.Errorf("outcome = %+v, want failed containing %q", got, tt.wantIn)
			}
			if f.ran() {
				t.Errorf("CLI was spawned for a rejected task")
			}
		})
	}
}

func TestRunWithoutCwdUsesPrivateScratch(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"ok"}`)
	if _, err := f.runner.Run(context.Background(), Task{ID: "task-9", Agent: "dev", Body: "b"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	scratch := filepath.Join(f.runner.ScratchDir, "task-9")
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("scratch mode = %v, want 0700", info.Mode().Perm())
	}
	if got := strings.TrimSpace(f.recorded(t, "cwd")); got != realpath(t, scratch) {
		t.Errorf("ran in %q, want %q", got, scratch)
	}
}

// waitGone polls until pid no longer exists. A killed process lingers until it
// is reaped, which can take a while on a loaded machine, so the wait is long;
// it returns as soon as the process is gone.
func waitGone(pid int) bool {
	for range 200 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// childPIDIfStarted reports the fake's background child, if it got that far.
func childPIDIfStarted(f fixture) (int, bool) {
	b, err := os.ReadFile(filepath.Join(f.dir, "child.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil
}

func childPID(t *testing.T, f fixture) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(f.recorded(t, "child.pid")))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestRunTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	f := newFixture(t)
	// The fake starts its child before anything else, but only once bash is
	// running; the timeout must comfortably outlast that startup on a loaded
	// machine, or child.pid would not exist yet when the group is killed.
	f.runner.Timeout = 2 * time.Second
	t.Setenv("FAKE_SLEEP", "30")
	t.Setenv("FAKE_CHILD", "1")

	start := time.Now()
	got, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != StatusFailed || !strings.Contains(got.Result, "timed out") {
		t.Errorf("outcome = %+v, want a timeout failure", got)
	}
	// An unkilled fake would sleep the full 30s; the bound leaves room for the
	// timeout plus the 10s WaitDelay grace without depending on machine speed.
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("Run took %s; the timeout did not stop it", elapsed)
	}
	// If the timeout fired before the fake got as far as starting its child,
	// there is no process that could have leaked; otherwise the child must die
	// with the group.
	if pid, ok := childPIDIfStarted(f); ok && !waitGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("a tool process the session spawned outlived the timeout")
	}
}

func TestRunShutdownAbandonsTheTask(t *testing.T) {
	f := newFixture(t)
	stop, cancel := context.WithCancel(context.Background())
	f.runner.Stop = stop
	t.Setenv("FAKE_SLEEP", "30")
	t.Setenv("FAKE_CHILD", "1")
	go func() {
		for !f.ran() {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()

	_, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one wrapping context.Canceled so the task is abandoned", err)
	}
	if pid := childPID(t, f); !waitGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("session tools outlived the shutdown")
	}
}

func TestSessionIDIsStablePerTask(t *testing.T) {
	if SessionID("task-a") != SessionID("task-a") {
		t.Errorf("session ID is not deterministic")
	}
	if SessionID("task-a") == SessionID("task-b") {
		t.Errorf("distinct tasks share a session ID")
	}
}

func TestValidAgent(t *testing.T) {
	for name, want := range map[string]bool{
		"dev": true, "adversarial-qa": true, "feature_dev": true,
		"": false, "-x": false, "--agent": false, "Dev": false, "a/b": false, strings.Repeat("a", 65): false,
	} {
		if got := ValidAgent(name); got != want {
			t.Errorf("ValidAgent(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFindClaude(t *testing.T) {
	onPath := t.TempDir()
	bin := filepath.Join(onPath, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		configured string
		path       string
		want       string
	}{
		{name: "configured path wins", configured: "/custom/claude", path: onPath, want: "/custom/claude"},
		{name: "found on PATH", path: onPath, want: bin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			if got := FindClaude(tt.configured, t.TempDir()); got != tt.want {
				t.Errorf("FindClaude = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("falls back to the user install location", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		home := t.TempDir()
		local := filepath.Join(home, ".local", "bin", "claude")
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := FindClaude("", home)
		// A Homebrew install on the test machine legitimately takes precedence.
		if got != local && !strings.HasPrefix(got, "/opt/homebrew/") && !strings.HasPrefix(got, "/usr/local/") {
			t.Errorf("FindClaude = %q, want %q", got, local)
		}
	})
}

func TestRunKillsLeftoversAfterANormalExit(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"result":"done"}`)
	t.Setenv("FAKE_CHILD", "1") // backgrounds a tool and exits without waiting for it

	got, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil || got.Status != StatusComplete {
		t.Fatalf("Run = %+v, %v", got, err)
	}
	if pid := childPID(t, f); !waitGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("a backgrounded tool outlived its session")
	}
}

func TestOutcomeTruncatesOnARuneBoundary(t *testing.T) {
	text := strings.Repeat("x", maxResultBytes-1) + "é" + "tail"
	got := outcome(StatusComplete, text).Result
	if !utf8.ValidString(got) {
		t.Fatalf("truncated result is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "[truncated]") || strings.Contains(got, "tail") {
		t.Errorf("result not truncated as expected")
	}
}

func TestValidPermissionMode(t *testing.T) {
	for mode, want := range map[string]bool{
		"auto": true, "dontAsk": true, "acceptEdits": true, "plan": true,
		"bypassPermissions": false, "manual": false, "": false,
	} {
		if got := ValidPermissionMode(mode); got != want {
			t.Errorf("ValidPermissionMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestRunPassesAllowedTools(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		want    string // the --allowedTools value; empty means the flag is absent
	}{
		{name: "none configured", allowed: nil, want: ""},
		{name: "rules joined into one value", allowed: []string{"Bash(git status:*)", " Bash(go test:*) "}, want: "Bash(git status:*),Bash(go test:*)"},
		{name: "blank and comma-bearing rules are dropped", allowed: []string{"", "Bash(a,b)", "Bash(ls:*)"}, want: "Bash(ls:*)"},
		{name: "a rule shaped like a flag is dropped", allowed: []string{"--dangerously-skip-permissions", "Bash(ls:*)"}, want: "Bash(ls:*)"},
		{name: "only flag-shaped rules leaves the flag off", allowed: []string{"-x"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.reply(t, `{"result":"ok"}`)
			f.runner.AllowedTools = tt.allowed
			if _, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n")
			i := slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, "--allowedTools") })
			if tt.want == "" {
				if i >= 0 {
					t.Errorf("unexpected %q in %q", args[i], args)
				}
				return
			}
			if i < 0 || args[i] != "--allowedTools="+tt.want {
				t.Fatalf("args = %q, want --allowedTools=%s as one token", args, tt.want)
			}
			// The agent flag still closes the option list, after the rules.
			if a := slices.Index(args, "--agent"); a < i {
				t.Errorf("--agent should follow --allowedTools: %q", args)
			}
			if d := slices.Index(args, "--disallowedTools"); d < 0 {
				t.Errorf("dispatch tools are no longer denied: %q", args)
			}
		})
	}
}

func TestRunRecordsMetrics(t *testing.T) {
	f := newFixture(t)
	f.reply(t, `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.4213,"num_turns":17,
		"session_id":"`+SessionID("task-m")+`","modelUsage":{"claude-sonnet-5-5":{},"claude-haiku-4-5":{}},
		"result":"Opened https://github.com/acme/widget/pull/42 and https://github.com/acme/api/pull/7; see https://github.com/acme/widget/pull/42 again."}`)
	got, err := f.runner.Run(context.Background(), Task{ID: "task-m", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != 0.4213 || got.Turns != 17 || got.SessionID != SessionID("task-m") {
		t.Errorf("metrics = %+v", got.Metrics)
	}
	if !slices.Equal(got.Models, []string{"claude-haiku-4-5", "claude-sonnet-5-5"}) {
		t.Errorf("models = %v, want sorted", got.Models)
	}
	if !slices.Equal(got.PRs, []string{"https://github.com/acme/widget/pull/42", "https://github.com/acme/api/pull/7"}) {
		t.Errorf("prs = %v, want each once in order", got.PRs)
	}
	if got.DurationMS <= 0 {
		t.Errorf("duration not measured: %d", got.DurationMS)
	}
}

func TestRunMeasuresFailedRuns(t *testing.T) {
	f := newFixture(t)
	f.runner.Timeout = 200 * time.Millisecond
	t.Setenv("FAKE_SLEEP", "30")
	got, err := f.runner.Run(context.Background(), Task{ID: "t", Agent: "dev", Body: "b", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.DurationMS < 200 {
		t.Errorf("a timed-out run must still record how long it took: %+v", got)
	}
}

func TestDeniedTools(t *testing.T) {
	if got := deniedTools("dev"); got != dispatchTools {
		t.Errorf("dev denied %q", got)
	}
	got := deniedTools("pr-verdict")
	for _, want := range []string{dispatchTools, "Bash(gh pr review:*)", "Bash(gh pr merge:*)", "Bash(gh api:*)", "Bash(git push:*)"} {
		if !strings.Contains(got, want) {
			t.Errorf("pr-verdict denied %q, missing %q", got, want)
		}
	}
}
