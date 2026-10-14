package agentrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCodex stands in for the Codex CLI. `mcp list --json` prints
// $FAKE_MCP_JSON (default: no servers) and exits $FAKE_MCP_EXIT. An exec run
// records its invocation into $FAKE_DIR, writes $FAKE_LAST to the -o file, and
// replays $FAKE_DIR/stdout and $FAKE_EXIT.
const fakeCodex = `#!/bin/bash
d="$FAKE_DIR"
if [ "$1" = mcp ] && [ "$2" = list ]; then
  if [ -n "$FAKE_MCP_BIG" ]; then head -c 2097152 /dev/zero | tr '\0' ' '; exit 0; fi
  printf '%s' "${FAKE_MCP_JSON:-[]}"
  exit "${FAKE_MCP_EXIT:-0}"
fi
printf '%s\n' "$@" > "$d/args"
cat > "$d/stdin"
pwd -P > "$d/cwd"
env > "$d/env"
out=""; prev=""
for a in "$@"; do [ "$prev" = -o ] && out="$a"; prev="$a"; done
if [ -n "$out" ] && [ -n "$FAKE_LAST" ]; then printf '%s' "$FAKE_LAST" > "$out"; fi
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
[ -f "$d/stdout" ] && cat "$d/stdout"
exit "${FAKE_EXIT:-0}"
`

const codexOK = `{"type":"thread.started","thread_id":"0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"opened https://github.com/acme/w/pull/9"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":5}}
`

func codexFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"FAKE_MCP_JSON", "FAKE_MCP_EXIT", "FAKE_MCP_BIG", "FAKE_LAST"} {
		t.Setenv(k, "")
	}
	orch := filepath.Join(t.TempDir(), "orch")
	if err := os.WriteFile(orch, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ORCH", orch)
	f.runner.Codex = CodexOptions{Bin: bin, Sandbox: "workspace-write", StateDir: filepath.Join(f.home, "codex"),
		MCPAllow: []string{"orch_rag"}, OrchBin: orch}
	return f
}

func codexTask(cwd string) Task {
	return Task{ID: "task-1", Agent: "dev", Body: "-x fix it", Cwd: cwd, Provider: ProviderCodex, Model: "gpt-sol", Effort: "high"}
}

func argAfter(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestCodexRunFresh(t *testing.T) {
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "opened https://github.com/acme/w/pull/9")
	t.Setenv("FAKE_MCP_JSON", `[{"name":"orch_queue","enabled":true,"transport":{"env":{"SECRET":"x"}}},`+
		`{"name":"orch_rag","enabled":true,"transport":{"command":"`+f.runner.Codex.OrchBin+`"}},`+
		`{"name":"github","enabled":true},{"name":"nostate"}]`)
	cwd := t.TempDir()
	got, err := f.runner.Run(context.Background(), codexTask(cwd))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusComplete || got.Result != "opened https://github.com/acme/w/pull/9" {
		t.Fatalf("outcome = %+v", got)
	}
	if got.Turns != 1 || got.SessionID != "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" ||
		!slices.Equal(got.Models, []string{"gpt-sol"}) || !slices.Equal(got.PRs, []string{"https://github.com/acme/w/pull/9"}) {
		t.Fatalf("metrics = %+v", got.Metrics)
	}
	args := strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n")
	if args[0] != "exec" || args[len(args)-1] != "-" {
		t.Fatalf("args = %q", args)
	}
	for _, want := range []string{"--json", "--skip-git-repo-check", "--ignore-rules"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %q", want, args)
		}
	}
	cfg := strings.Join(argAfter(args, "-c"), "\n")
	for _, want := range []string{`approval_policy="never"`, `sandbox_mode="workspace-write"`,
		"sandbox_workspace_write.network_access=false", "sandbox_workspace_write.writable_roots=[]", "notify=[]",
		"shell_environment_policy.ignore_default_excludes=false", `shell_environment_policy.set.ORCH_TASK_ID="task-1"`,
		"mcp_servers.orch_queue.enabled=false", "mcp_servers.github.enabled=false", "mcp_servers.nostate.enabled=false",
		`model_reasoning_effort="high"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config overrides lack %s:\n%s", want, cfg)
		}
	}
	for _, kept := range []string{"mcp_servers.orch_rag", "SECRET"} {
		if strings.Contains(cfg, kept) {
			t.Errorf("config overrides touch %s:\n%s", kept, cfg)
		}
	}
	if m := argAfter(args, "-m"); !slices.Equal(m, []string{"gpt-sol"}) {
		t.Errorf("-m = %q", m)
	}
	if c := argAfter(args, "-C"); len(c) != 1 || c[0] != cwd {
		t.Errorf("-C = %q", c)
	}
	for _, bad := range []string{"--dangerously-bypass-approvals-and-sandbox", "danger-full-access", "-x fix it"} {
		if slices.Contains(args, bad) || strings.Contains(cfg, bad) {
			t.Errorf("argv carries %q", bad)
		}
	}
	if stdin := f.recorded(t, "stdin"); !strings.Contains(stdin, "-x fix it") {
		t.Errorf("prompt not on stdin: %q", stdin)
	}
	if env := f.recorded(t, "env"); !strings.Contains(env, TaskIDEnv+"=task-1\n") {
		t.Error("session env lacks the task id")
	}
	// The task finished, so there is nothing left to resume.
	if th := f.runner.codexThread("task-1"); th != "" {
		t.Fatalf("finished task kept its thread: %q", th)
	}
	if entries, _ := os.ReadDir(f.runner.Codex.StateDir); len(entries) != 0 {
		t.Fatalf("state dir not cleaned: %v", entries)
	}
}

func TestCodexMCPListFailsClosed(t *testing.T) {
	tests := []struct {
		name, json, exit, want string
	}{
		{name: "list fails", exit: "3", want: "list codex MCP servers"},
		{name: "unreadable list", json: "not json", want: "unreadable output"},
		{name: "unsafe name", json: `[{"name":"a.b","enabled":true}]`, want: "cannot be turned off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := codexFixture(t)
			t.Setenv("FAKE_MCP_JSON", tt.json)
			t.Setenv("FAKE_MCP_EXIT", tt.exit)
			got, err := f.runner.Run(context.Background(), codexTask(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != StatusFailed || !strings.Contains(got.Result, tt.want) {
				t.Fatalf("outcome = %+v", got)
			}
			if f.ran() {
				t.Fatal("codex ran with MCP servers it could not turn off")
			}
		})
	}
}

func TestCodexRunResumesItsThread(t *testing.T) {
	f := codexFixture(t)
	f.runner.saveCodexThread("task-1", "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b")
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "done")
	cwd := t.TempDir()
	got, err := f.runner.Run(context.Background(), codexTask(cwd))
	if err != nil || got.Status != StatusComplete {
		t.Fatalf("outcome = %+v, %v", got, err)
	}
	args := strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n")
	if args[0] != "exec" || args[1] != "resume" || args[len(args)-2] != "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" || args[len(args)-1] != "-" {
		t.Fatalf("args = %q", args)
	}
	if slices.Contains(args, "-C") {
		t.Error("resume does not take -C; the directory comes from the process")
	}
	// Without the queue server configured, it is not named (that would fail Codex).
	if strings.Contains(strings.Join(args, " "), "mcp_servers") {
		t.Errorf("args name an unconfigured MCP server: %q", args)
	}
	if strings.TrimSpace(f.recorded(t, "cwd")) != realpath(t, cwd) {
		t.Error("resume ran in the wrong directory")
	}
	if !strings.Contains(f.recorded(t, "stdin"), "orchd restarted") {
		t.Error("resume did not send the resume prompt")
	}
}

func TestCodexOutcomes(t *testing.T) {
	tests := []struct {
		name, stdout, last string
		exit               string
		want               string
		wantIn             string
	}{
		{name: "blocked", stdout: codexOK, last: "BLOCKED: need a token", want: StatusBlocked},
		{name: "turn failed", stdout: `{"type":"turn.failed","error":{"message":"rate limited"}}`, want: StatusFailed, wantIn: "rate limited"},
		{name: "error event", stdout: `{"type":"error","message":"stream broke"}`, want: StatusFailed, wantIn: "stream broke"},
		{name: "non-zero exit", stdout: codexOK, last: "partial", exit: "2", want: StatusFailed, wantIn: "codex exited"},
		{name: "no final message", stdout: `{"type":"turn.completed","usage":{}}`, want: StatusFailed, wantIn: "without a final message"},
		{name: "falls back to the last agent message", stdout: codexOK, want: StatusComplete, wantIn: "pull/9"},
		{name: "garbage lines are skipped", stdout: "not json\n" + codexOK, last: "ok", want: StatusComplete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := codexFixture(t)
			f.reply(t, tt.stdout)
			t.Setenv("FAKE_LAST", tt.last)
			t.Setenv("FAKE_EXIT", tt.exit)
			got, err := f.runner.Run(context.Background(), codexTask(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.want || !strings.Contains(got.Result, tt.wantIn) {
				t.Fatalf("outcome = %+v", got)
			}
		})
	}
}

func TestCodexRefusesUnsafeSetup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Runner, *Task)
		want   string
	}{
		{name: "no binary", mutate: func(r *Runner, _ *Task) { r.Codex.Bin = "" }, want: "no Codex CLI"},
		{name: "full access", mutate: func(r *Runner, _ *Task) { r.Codex.Sandbox = "danger-full-access" }, want: "not allowed"},
		{name: "empty sandbox", mutate: func(r *Runner, _ *Task) { r.Codex.Sandbox = "" }, want: "not allowed"},
		{name: "bad effort", mutate: func(_ *Runner, t *Task) { t.Effort = `high" -c x="y` }, want: "invalid effort"},
		{name: "bad model", mutate: func(_ *Runner, t *Task) { t.Model = "--oss" }, want: "invalid model"},
		{name: "bad task id", mutate: func(_ *Runner, t *Task) { t.ID = `t"1` }, want: "invalid task id"},
		{name: "no state dir", mutate: func(r *Runner, _ *Task) { r.Codex.StateDir = "" }, want: "state directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := codexFixture(t)
			task := codexTask(t.TempDir())
			tt.mutate(f.runner, &task)
			got, err := f.runner.Run(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != StatusFailed || !strings.Contains(got.Result, tt.want) {
				t.Fatalf("outcome = %+v", got)
			}
			if f.ran() {
				t.Fatal("codex ran despite the refusal")
			}
		})
	}
}

func TestCodexThreadFileIsValidated(t *testing.T) {
	f := codexFixture(t)
	if err := os.MkdirAll(f.runner.Codex.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.runner.codexThreadFile("task-1"), []byte("--last\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if th := f.runner.codexThread("task-1"); th != "" {
		t.Fatalf("flag-shaped thread accepted: %q", th)
	}
	f.runner.saveCodexThread("task-2", "bad thread")
	if th := f.runner.codexThread("task-2"); th != "" {
		t.Fatalf("invalid thread saved: %q", th)
	}
	if th := f.runner.codexThread("../x"); th != "" {
		t.Fatal("path-shaped task id read a thread")
	}
}

func TestCodexInterruptedRunKeepsItsThread(t *testing.T) {
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_SLEEP", "30")
	stop, cancel := context.WithCancel(context.Background())
	f.runner.Stop = stop
	// The fake prints only after sleeping, so emit the thread first via a
	// wrapper that writes stdout before blocking.
	wrapper := filepath.Join(t.TempDir(), "codex")
	script := strings.Replace(fakeCodex, `if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
[ -f "$d/stdout" ] && cat "$d/stdout"`, `[ -f "$d/stdout" ] && cat "$d/stdout"
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi`, 1)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.runner.Codex.Bin = wrapper
	done := make(chan error, 1)
	go func() { _, err := f.runner.Run(context.Background(), codexTask(t.TempDir())); done <- err }()
	deadline := time.Now().Add(codexWait)
	for !f.ran() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let the thread line reach the capture file
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted run returned no error")
		}
	case <-time.After(codexWait):
		t.Fatal("run did not stop")
	}
	if th := f.runner.codexThread("task-1"); th == "" {
		t.Fatal("an interrupted run lost its thread, so it cannot resume")
	}
}

// codexRepo makes a repository with one commit, isolated from user git config.
func codexRepo(t *testing.T) (dir string, git func(dir string, args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	git = func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	dir = t.TempDir()
	git(dir, "init", "-q", "-b", "main")
	git(dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	return dir, git
}

func TestCodexWritableRoots(t *testing.T) {
	repo, git := codexRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", wt)
	r := &Runner{}
	ctx := context.Background()
	common := realpath(t, filepath.Join(repo, ".git"))

	roots := r.codexWritableRoots(ctx, wt)
	want := []string{filepath.Join(common, "worktrees", "wt"), filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")}
	if !slices.Equal(roots, want) {
		t.Fatalf("worktree roots = %q, want %q", roots, want)
	}
	for _, root := range roots {
		if root == common {
			t.Fatal("the whole git directory, hooks and config included, was made writable")
		}
	}
	// A plain clone gets nothing: Codex keeps its .git read-only.
	if roots := r.codexWritableRoots(ctx, repo); roots != nil {
		t.Fatalf("plain clone roots = %q", roots)
	}
	// A subdirectory of a worktree is not its top.
	sub := filepath.Join(wt, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if roots := r.codexWritableRoots(ctx, sub); roots != nil {
		t.Fatalf("subdirectory roots = %q", roots)
	}
	if roots := r.codexWritableRoots(ctx, t.TempDir()); roots != nil {
		t.Fatalf("non-repo roots = %q", roots)
	}
}

// A session could plant a .git file whose gitdir carries a commondir naming
// another repository; that repository's git directory must not become writable.
func TestCodexWritableRootsRejectRedirectedGitDir(t *testing.T) {
	victim, _ := codexRepo(t)
	attacker := t.TempDir()
	fake := filepath.Join(attacker, "fakegit")
	for _, d := range []string{"refs", "objects"} {
		if err := os.MkdirAll(filepath.Join(fake, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(fake, "HEAD"):      "ref: refs/heads/main\n",
		filepath.Join(fake, "commondir"): filepath.Join(victim, ".git") + "\n",
		filepath.Join(fake, "gitdir"):    filepath.Join(attacker, "checkout", ".git") + "\n",
	}
	checkout := filepath.Join(attacker, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	files[filepath.Join(checkout, ".git")] = "gitdir: " + fake + "\n"
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{}
	if roots := r.codexWritableRoots(context.Background(), checkout); roots != nil {
		t.Fatalf("redirected git dir granted roots %q", roots)
	}
}

func TestCodexWorktreeRootsReachTheRun(t *testing.T) {
	repo, git := codexRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", wt)
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "ok")
	if _, err := f.runner.Run(context.Background(), codexTask(wt)); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Join(argAfter(strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n"), "-c"), "\n")
	objects := tomlString(filepath.Join(realpath(t, filepath.Join(repo, ".git")), "objects"))
	if !strings.Contains(cfg, "sandbox_workspace_write.writable_roots=[") || !strings.Contains(cfg, objects) {
		t.Fatalf("writable roots missing from:\n%s", cfg)
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b":        `"/a/b"`,
		`a"b\c`:       `"a\"b\\c"`,
		"tab\there":   `"tab\u0009here"`,
		"/naïve/path": `"/naïve/path"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
	if got := tomlStrings(nil); got != "[]" {
		t.Errorf("empty = %s", got)
	}
}

func TestParseCodexEventsSkipsOversizedLines(t *testing.T) {
	big := `{"type":"item.completed","item":{"type":"agent_message","text":"` + strings.Repeat("x", maxEventLine) + `"}}`
	ev := parseCodexEvents([]byte(big + "\n" + `{"type":"turn.failed","error":{"message":"later failure"}}`))
	if ev.failure != "later failure" {
		t.Fatalf("a long line hid a later failure: %+v", ev.failure)
	}
	if ev := parseCodexEvents([]byte(`{"type":"thread.started","thread_id":"abc"}`)); ev.thread != "abc" {
		t.Fatalf("unterminated last line lost: %+v", ev)
	}
}

func TestFindCodex(t *testing.T) {
	if got := FindCodex("/opt/x/codex", ""); got != "/opt/x/codex" {
		t.Fatalf("configured = %q", got)
	}
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(home, ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindCodex("", home); got != bin && !strings.HasPrefix(got, "/opt/homebrew") && !strings.HasPrefix(got, "/usr/local") {
		t.Fatalf("found %q", got)
	}
}

func TestSearchPathSkipsMissingBinaries(t *testing.T) {
	r := &Runner{Codex: CodexOptions{Bin: "/opt/codex/bin/codex"}}
	path := filepath.SplitList(r.searchPath())
	if slices.Contains(path, ".") || slices.Contains(path, "") {
		t.Fatalf("PATH includes the working directory: %q", path)
	}
	if !slices.Contains(path, "/opt/codex/bin") {
		t.Fatalf("PATH lacks the codex directory: %q", path)
	}
}

// codexWait bounds positive waits in these tests.
const codexWait = 30 * time.Second

func TestCodexAllowlistChecksTheCommand(t *testing.T) {
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "ok")
	// An allowed name running some other command is a repository reusing it.
	t.Setenv("FAKE_MCP_JSON", `[{"name":"orch_rag","enabled":true,"transport":{"command":"/tmp/evil"}}]`)
	if _, err := f.runner.Run(context.Background(), codexTask(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Join(argAfter(strings.Split(strings.TrimSpace(f.recorded(t, "args")), "\n"), "-c"), "\n")
	if !strings.Contains(cfg, "mcp_servers.orch_rag.enabled=false") {
		t.Fatalf("impostor orch_rag kept:\n%s", cfg)
	}
	if (&Runner{}).isOrch("/bin/sh") || (&Runner{Codex: CodexOptions{OrchBin: "/bin/sh"}}).isOrch("sh") {
		t.Fatal("isOrch accepted a non-matching or relative command")
	}
}

func TestCodexListingTooLarge(t *testing.T) {
	f := codexFixture(t)
	t.Setenv("FAKE_MCP_BIG", "1")
	got, err := f.runner.Run(context.Background(), codexTask(t.TempDir()))
	if err != nil || got.Status != StatusFailed || !strings.Contains(got.Result, "too large") {
		t.Fatalf("outcome = %+v, %v", got, err)
	}
}

func TestCodexRefusesProjectConfig(t *testing.T) {
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "ok")
	repo := filepath.Join(f.home, "code", "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := f.runner.Run(context.Background(), codexTask(sub))
	if err != nil || got.Status != StatusFailed || !strings.Contains(got.Result, "project config") || f.ran() {
		t.Fatalf("outcome = %+v, %v (ran=%v)", got, err, f.ran())
	}
	f.runner.Codex.AllowProjectConfig = true
	if got, _ := f.runner.Run(context.Background(), codexTask(sub)); got.Status != StatusComplete {
		t.Fatalf("opted in, outcome = %+v", got)
	}
	// A .codex in the home directory itself is the user's own config, not a project's.
	if pc := projectConfig(filepath.Join(f.home, "elsewhere"), f.home); pc != "" {
		t.Fatalf("projectConfig = %q", pc)
	}
}

// The fake session tampers the way a steered one could: it repoints its
// worktree's commondir, moves main, adds a tag and a branch.
func TestCodexGitGuardRestoresTampering(t *testing.T) {
	repo, git := codexRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", "-b", "task-branch", wt)
	common := filepath.Join(repo, ".git")
	gitDir := filepath.Join(common, "worktrees", "wt")
	before, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		t.Fatal(err)
	}
	mainBefore := strings.TrimSpace(gitOut(t, repo, "rev-parse", "main"))

	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "ok")
	tamper := filepath.Join(t.TempDir(), "codex")
	script := strings.Replace(fakeCodex, `[ -f "$d/stdout" ] && cat "$d/stdout"`, `printf '/tmp/evil\n' > "`+gitDir+`/commondir.new" && mv "`+gitDir+`/commondir.new" "`+gitDir+`/commondir"
printf 'x' > "`+gitDir+`/config.worktree"
GIT_DIR="`+common+`" git update-ref refs/heads/main 4b825dc642cb6eb9a060e54bf8d69288fbee4904 2>/dev/null || printf '%s\n' 0000000000000000000000000000000000000001 > "`+common+`/refs/heads/main"
printf '%s\n' "`+mainBefore+`" > "`+common+`/refs/tags/planted"
printf '%s\n' "`+mainBefore+`" > "`+common+`/refs/heads/agent-side-branch"
[ -f "$d/stdout" ] && cat "$d/stdout"`, 1)
	if err := os.WriteFile(tamper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.runner.Codex.Bin = tamper
	got, err := f.runner.Run(context.Background(), codexTask(wt))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || !strings.Contains(got.Result, "changed git state") {
		t.Fatalf("outcome = %+v", got)
	}
	for _, want := range []string{"worktree commondir", "worktree config.worktree", "refs/heads/main", "refs/tags/planted"} {
		if !strings.Contains(got.Result, want) {
			t.Errorf("result does not name %s: %s", want, got.Result)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(gitDir, "commondir")); string(after) != string(before) {
		t.Fatalf("commondir not restored: %q", after)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config.worktree")); err == nil {
		t.Fatal("planted config.worktree not removed")
	}
	if now := strings.TrimSpace(gitOut(t, repo, "rev-parse", "main")); now != mainBefore {
		t.Fatalf("main not restored: %s, want %s", now, mainBefore)
	}
	if _, err := os.Stat(filepath.Join(common, "refs", "tags", "planted")); err == nil {
		t.Fatal("planted tag not removed")
	}
	if strings.Contains(got.Result, "agent-side-branch") {
		t.Fatal("a new branch was treated as tampering")
	}
}

func TestCodexGitGuardQuietOnCleanRun(t *testing.T) {
	repo, git := codexRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", "-b", "task-branch", wt)
	f := codexFixture(t)
	f.reply(t, codexOK)
	t.Setenv("FAKE_LAST", "ok")
	if got, err := f.runner.Run(context.Background(), codexTask(wt)); err != nil || got.Status != StatusComplete {
		t.Fatalf("outcome = %+v, %v", got, err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
