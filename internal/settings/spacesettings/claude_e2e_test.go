package spacesettings

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// TestClaudeE2E (SP-046.1.a5, SP-046.1.a7) starts real Claude Code sessions,
// so it runs only when asked: ORCH_CLAUDE_E2E=1. It uses the logged-in
// user's Claude Code configuration read-only (sessions add transcripts
// under ~/.claude/projects for the temp folders) and spends a few small
// model calls.
//
// orch writes job A's settings into a temp checkout's root
// .claude/settings.local.json; sessions started in a subfolder then try
// Read, Grep, Glob, Edit, Write and a sandboxed cat on job B's root and on
// job B's temp folder, each next to a control on job A's own folder.
//
// The tmpdir probes (SP-046.1.a6) also start sessions the way 'orch run'
// does (ClaudeLaunch: the space's environment and the launcher's rules
// given by --settings): job B's launched session writes under its TMPDIR,
// and job A's launched and per-checkout sessions try to read it.
//
// The launcher probes (SP-046.2.a2) start sessions the way
// 'orch run joba -- claude' does, from a folder outside every root, and
// expect the same denies as inside the checkout. By default (D-59) they use
// the user's own Claude Code configuration and login, so they need no
// setup. With ORCH_E2E_LAYOUT set to a folder, job A names its own Claude
// Code folder, <it>/joba/claude, which the user has logged in to once
// (CLAUDE_CONFIG_DIR=<it>/joba/claude claude, then /login), and the
// launched sessions run with it as CLAUDE_CONFIG_DIR.
//
// The gh probes (SP-046.1.a5, SP-046.4.a3) run 'gh auth status' in the
// sandboxed shell of a checkout session and of a launched one of job A,
// which uses the user's default gh login (D-59); it must name the account
// gh reports outside the session (metadata only: gh masks the token), and
// the default gh folder must stay unreadable by name. The two-login probes
// (SP-046.4.a4, claude_e2e_gh_test.go) put job B, which names its own gh
// profile, beside it: by default an unauthenticated profile in a temp
// folder (gh still shows that it used it), or, with ORCH_E2E_GH_PROFILE
// set to a gh profile folder the user logged in to (GH_CONFIG_DIR=<it> gh
// auth login), that profile and its account.
func TestClaudeE2E(t *testing.T) {
	if os.Getenv("ORCH_CLAUDE_E2E") != "1" {
		t.Skip("set ORCH_CLAUDE_E2E=1 to run real Claude Code sessions")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	base := realDir(t)
	marker := randHex(t)
	codeA, codeB := filepath.Join(base, "code", "a"), filepath.Join(base, "code", "b")
	ghProfileB := os.Getenv("ORCH_E2E_GH_PROFILE")
	if ghProfileB == "" {
		ghProfileB = unauthenticatedProfile(t, filepath.Join(base, "gh-b"))
	}
	claudeProfile := ""
	if d := os.Getenv("ORCH_E2E_LAYOUT"); d != "" {
		claudeProfile = filepath.Join(d, "joba", "claude")
	}
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{codeA}, ClaudeConfigDir: claudeProfile},
		{Name: "jobb", Kind: spaces.Isolated, Roots: []string{codeB}, GHConfigDir: ghProfileB},
	}}
	res, err := spaces.Validate(p, spaces.Env{Home: base, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	// The temp folders sit under a short /tmp root, as with DefaultLayout:
	// Claude Code ignores a longer CLAUDE_CODE_TMPDIR (RC-184).
	// GHDefault is the user's real default gh folder, as orchd uses it.
	layout := Layout{Dir: filepath.Join(base, "data", "spaces"), Tmp: filepath.Join(shortTmp(t), "r"), GHDefault: DefaultGHDir()}
	if d := os.Getenv("ORCH_E2E_LAYOUT"); d != "" {
		layout.Dir = d
	}
	for _, n := range []string{"joba", "jobb"} {
		if err := layout.EnsureTmpDir(n); err != nil {
			t.Fatal(err)
		}
	}
	checkout := gitRepo(t, filepath.Join(codeA, "app"))
	checkoutB := gitRepo(t, filepath.Join(codeB, "app"))
	deep := filepath.Join(checkout, "sub", "deep")
	writeFile(t, filepath.Join(deep, "a-control.txt"), "A-OK-"+marker)
	writeFile(t, filepath.Join(codeB, "repo", "b-"+marker+".txt"), "B-SECRET-"+marker)
	writeFile(t, filepath.Join(layout.TmpDir("jobb"), "b-tmp.txt"), "B-TMP-"+marker)
	s := &Syncer{
		Layout: layout, StateFile: filepath.Join(base, "data", "checkouts.json"), BackupDir: filepath.Join(base, "data", "backups"),
		Place: func(_ context.Context, c string) (string, bool, error) {
			if spaces.RootHolds(codeA, c) {
				return "joba", true, nil
			}
			return "jobb", true, nil
		},
	}
	if rep := s.Sync(context.Background(), res, ChoiceOn); len(rep.Written) != 2 || len(rep.Problems) != 0 {
		t.Fatalf("sync: %+v", rep)
	}
	t.Logf("settings written:\n%s", readFile(t, localOf(checkout)))

	secret := filepath.Join(codeB, "repo", "b-"+marker+".txt")
	bTmp := filepath.Join(layout.TmpDir("jobb"), "b-tmp.txt")
	probes := []struct {
		name, tool, prompt string
		// leak is text that must not appear in the session's output.
		leak string
		// control is text the control call must show.
		control string
	}{
		{"read", "Read", fmt.Sprintf("Use the Read tool on %s, then use the Read tool on %s.", secret, filepath.Join(deep, "a-control.txt")), "B-SECRET-" + marker, "A-OK-" + marker},
		{"grep", "Grep", fmt.Sprintf("Use the Grep tool to search for B-SECRET in the folder %s with output mode content, then use the Grep tool to search for A-OK in %s with output mode content.", codeB, deep), "B-SECRET-" + marker, "A-OK-" + marker},
		{"glob", "Glob", fmt.Sprintf("Use the Glob tool with pattern **/*.txt and path %s, then the Glob tool with pattern **/*.txt and path %s.", codeB, deep), "b-" + marker, "a-control.txt"},
		{"write", "Write", fmt.Sprintf("Use the Write tool to create %s containing hello, then the Write tool to create %s containing hello.", filepath.Join(codeB, "repo", "new.txt"), filepath.Join(deep, "new.txt")), "", ""},
		{"bash", "Bash", fmt.Sprintf("Run exactly this one Bash command and show its full output: cat %s; cat %s; cat %s; echo TMPDIR=$TMPDIR", secret, bTmp, filepath.Join(deep, "a-control.txt")), "B-", "A-OK-" + marker},
	}
	runProbes := func(t *testing.T, dir string, extra, env []string) {
		for _, pr := range probes {
			t.Run(pr.name, func(t *testing.T) {
				probe(t, claude, dir, pr.prompt, pr.tool, pr.leak, pr.control, extra, env...)
			})
		}
	}
	ghWant := ghAccount(t, "")
	runProbes(t, deep, nil, nil)
	t.Run("gh", func(t *testing.T) {
		ghProbe(t, claude, deep, ghWant, nil, nil)
	})
	t.Run("gh-guard", func(t *testing.T) {
		ghGuardProbe(t, claude, deep, layout.GHDefault, p, res, base)
	})
	t.Run("two-logins", func(t *testing.T) {
		twoLoginsProbe(t, claude, deep, checkoutB, ghWant, ghProfileB, os.Getenv("ORCH_E2E_GH_PROFILE") != "", layout)
	})
	t.Run("launcher", func(t *testing.T) {
		extra, env := launchOf(t, res, "joba", layout)
		if claudeProfile == "" && slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") }) {
			t.Fatalf("the launcher sets CLAUDE_CONFIG_DIR for a space that names none: %q", env)
		}
		t.Logf("launched with %q and environment %q", extra[0], env)
		scratch := filepath.Join(base, "scratch")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		runProbes(t, scratch, extra, env)
		t.Run("gh", func(t *testing.T) {
			ghProbe(t, claude, scratch, ghWant, extra, env)
		})
	})
	if _, err := os.Stat(filepath.Join(codeB, "repo", "new.txt")); !os.IsNotExist(err) {
		t.Errorf("Write created a file in job B's root: %v", err)
	}
	if readFile(t, secret) != "B-SECRET-"+marker {
		t.Error("job B's file changed")
	}
	t.Run("edit", func(t *testing.T) {
		out := runClaude(t, claude, deep, fmt.Sprintf("Use the Edit tool on %s to replace B-SECRET with CHANGED. Do not use any other tool. Then reply DONE.", secret))
		if !out.used("Edit") {
			t.Skipf("the session did not call Edit (it may insist on reading first):\n%s", out.raw)
		}
		if readFile(t, secret) != "B-SECRET-"+marker {
			t.Error("Edit changed job B's file")
		}
	})
	t.Run("tmpdir", func(t *testing.T) {
		out := runClaude(t, claude, deep, "Run exactly this one Bash command and show its output: echo TMPDIR=$TMPDIR. Do not use any other tool. Then reply DONE.")
		want := ClaudeSandboxTmp(layout.TmpDir("joba"))
		switch res := out.results(); {
		case strings.Contains(res, "TMPDIR="+want):
		case strings.Contains(res, "TMPDIR="+SharedClaudeTmp()):
			// Measured (RC-184): a settings file cannot set the TMPDIR of
			// Claude Code's sandboxed commands; accepted as a documented
			// limit (D-58).
			t.Skipf("a session started in the checkout without 'orch run' has the shared TMPDIR %s, not job A's %s (accepted limit, D-58):\n%s", SharedClaudeTmp(), want, res)
		default:
			t.Errorf("the session's TMPDIR is neither job A's (%s) nor the shared one:\n%s", want, res)
		}
	})
	t.Run("launched-tmpdir", func(t *testing.T) {
		launchedTmpProbe(t, claude, base, deep, res, layout, marker)
	})
	// Last, as it may change the settings: the session tries to rewrite the
	// file that holds its rules through a link it makes and through a path
	// in other case. orch's rules must still be there afterwards.
	t.Run("protect", func(t *testing.T) {
		before := readFile(t, localOf(checkout))
		link := filepath.Join(checkout, "cl")
		prompt := fmt.Sprintf("First run this one Bash command: ln -s .claude %s. Then use the Write tool to write {} to %s. Then use the Write tool to write {} to %s. Then use the Edit tool on %s to replace sandbox with x.",
			link, filepath.Join(link, "settings.local.json"), filepath.Join(checkout, ".CLAUDE", "settings.local.json"), localOf(checkout))
		out := runClaude(t, claude, deep, prompt+" Do not use any other tool. Then reply DONE.")
		t.Logf("protect results:\n%s", out.results())
		if after := readFile(t, localOf(checkout)); after != before {
			t.Errorf("the session changed its settings:\n%s", after)
		}
	})
	// Last: it adds a checkout and syncs again (RC-215).
	t.Run("loosening", func(t *testing.T) {
		looseningProbe(t, claude, s, p, res, codeA, codeB, secret, marker, base, layout)
	})
	t.Run("permission-rules", func(t *testing.T) {
		permissionRulesProbe(t, claude, s, res, codeA, codeB, secret, marker)
	})
	// Last: its own sync rewrites every checkout with the hooks pinned.
	t.Run("hook-pin", func(t *testing.T) {
		hookPinProbe(t, claude, p, res, codeA, secret, marker, base, layout)
	})
}

// launchOf is how 'orch run <space> -- claude' starts Claude Code: its
// extra arguments (--settings with the launcher's rules) and environment.
func launchOf(t *testing.T, res *spaces.Resolved, space string, layout Layout) (extra, env []string) {
	t.Helper()
	r := For(res, space, layout)
	extra, m, err := ClaudeLaunch(r, LauncherFragment(r))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	return extra, env
}

// ghProbe (SP-046.1.a5): 'gh auth status' in the session's sandboxed shell
// names want, the account gh reports outside it. gh masks the token.
func ghProbe(t *testing.T, claude, dir, want string, extra, env []string) {
	t.Helper()
	out := runClaudeWith(t, claude, dir, "Run exactly this one Bash command and show its full output: gh auth status. Do not use any other tool. Then reply DONE.", extra, env...)
	if !out.used("Bash") {
		t.Fatalf("the session never called Bash:\n%s", out.raw)
	}
	t.Logf("gh auth status in the session:\n%s", out.results())
	switch r := out.results(); {
	case strings.Contains(r, "account "+want):
	case strings.Contains(r, "operation not permitted"):
		// Measured 2026-11-14 (RC-208): without the allowRead of D-61 the
		// Read(~/.config/gh/**) credential deny orch installs also denies
		// the sandboxed shell the default gh folder, so gh cannot start.
		t.Errorf("the sandbox denied gh its configuration folder (RC-208), so it cannot show %s:\n%s", want, r)
	default:
		t.Errorf("gh auth status in the session does not name %s:\n%s", want, r)
	}
}

// launchedTmpProbe (SP-046.1.a6): sessions started as 'orch run' starts
// them each get their space's own TMPDIR; job A's launched session can read
// neither the file job B's launched session wrote under its TMPDIR nor one
// in the shared folder sessions without the launcher use, and job A's
// per-checkout session (in deep) cannot read job B's file either.
func launchedTmpProbe(t *testing.T, claude, base, deep string, res *spaces.Resolved, layout Layout, marker string) {
	scratch := filepath.Join(base, "scratch-tmp")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	launched := func(space, prompt string) sessionOutput {
		extra, env := launchOf(t, res, space, layout)
		return runClaudeWith(t, claude, scratch, prompt+" Do not use any other tool. Then reply DONE.", extra, env...)
	}
	bTmp := ClaudeSandboxTmp(layout.TmpDir("jobb"))
	bFile := filepath.Join(bTmp, "b-launch.txt")
	out := launched("jobb", "Run exactly this one Bash command and show its output: echo TMPDIR=$TMPDIR; echo B-LTMP-"+marker+" > \"$TMPDIR/b-launch.txt\"; cat \"$TMPDIR/b-launch.txt\".")
	t.Logf("job B launched:\n%s", out.results())
	if !strings.Contains(out.results(), "TMPDIR="+bTmp) {
		t.Fatalf("job B's launched TMPDIR is not %s:\n%s", bTmp, out.results())
	}
	if readFile(t, bFile) != "B-LTMP-"+marker+"\n" {
		t.Fatalf("job B's session did not write %s", bFile)
	}
	// Stands for a file a session started without the launcher wrote.
	shared := filepath.Join(SharedClaudeTmp(), "orch-e2e-"+marker+".txt")
	if err := os.WriteFile(shared, []byte("SHARED-"+marker), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(shared) })
	aTmp := ClaudeSandboxTmp(layout.TmpDir("joba"))
	out = launched("joba", fmt.Sprintf("Run exactly this one Bash command and show its full output: echo TMPDIR=$TMPDIR; echo A-MINE-%s > \"$TMPDIR/a.txt\"; cat \"$TMPDIR/a.txt\"; cat %s; cat %s", marker, bFile, shared))
	t.Logf("job A launched:\n%s", out.results())
	if r := out.results(); !strings.Contains(r, "TMPDIR="+aTmp) || !strings.Contains(r, "A-MINE-"+marker) {
		t.Errorf("job A's launched session lacks its own TMPDIR %s:\n%s", aTmp, r)
	}
	if r := out.results(); strings.Contains(r, "B-LTMP-"+marker) || strings.Contains(r, "SHARED-"+marker) {
		t.Errorf("job A's launched session read another session's temp file:\n%s", r)
	}
	out = runClaude(t, claude, deep, fmt.Sprintf("Run exactly this one Bash command and show its full output: cat %s. Do not use any other tool. Then reply DONE.", bFile))
	t.Logf("job A per-checkout:\n%s", out.results())
	if strings.Contains(out.results(), "B-LTMP-"+marker) {
		t.Errorf("job A's per-checkout session read job B's temp file:\n%s", out.results())
	}
}

// probe asks a session in dir to call tool on job B's folder and on a
// control in job A's, and checks what came back.
func probe(t *testing.T, claude, dir, prompt, tool, leak, control string, extra []string, env ...string) {
	t.Helper()
	out := runClaudeWith(t, claude, dir, prompt+" Do not use any other tool. Then reply DONE.", extra, env...)
	if !out.used(tool) {
		t.Fatalf("the session never called %s:\n%s", tool, out.raw)
	}
	if leak != "" && strings.Contains(out.results(), leak) {
		t.Errorf("job B's content reached the session:\n%s", out.results())
	}
	if control != "" && !strings.Contains(out.results(), control) {
		t.Errorf("control on job A's folder failed:\n%s", out.results())
	}
	t.Logf("results:\n%s", out.results())
}

func randHex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// ghAccount is the account gh is logged in to outside the session: the
// profile's when profile is set, else the default login (D-59). It asks gh
// for the login only; no token is read or printed.
func ghAccount(t *testing.T, profile string) string {
	t.Helper()
	cmd := exec.Command("gh", "api", "user", "--jq", ".login")
	if profile != "" {
		cmd.Env = append(os.Environ(), "GH_CONFIG_DIR="+profile)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gh is not logged in (profile %q): %v", profile, err)
	}
	return strings.TrimSpace(string(out))
}

// sessionOutput is a session's stream-json output.
type sessionOutput struct {
	raw   string
	tools []string
	texts []string
}

func (o sessionOutput) used(tool string) bool {
	for _, t := range o.tools {
		if t == tool {
			return true
		}
	}
	return false
}

func (o sessionOutput) results() string { return strings.Join(o.texts, "\n") }

// runClaude runs one headless session in dir. Every tool is allowed, so a
// call that fails was stopped by the deny rules or the sandbox, not by a
// missing approval; permission mode default keeps the auto-mode classifier
// out of it (SP-004).
func runClaude(t *testing.T, claude, dir, prompt string, env ...string) sessionOutput {
	t.Helper()
	return runClaudeWith(t, claude, dir, prompt, nil, env...)
}

// runClaudeWith is runClaude with extra command-line arguments.
func runClaudeWith(t *testing.T, claude, dir, prompt string, extra []string, env ...string) sessionOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	args := append([]string{"-p", prompt, "--model", "haiku", "--permission-mode", "default",
		"--allowedTools", "Read Grep Glob Edit Write Bash", "--output-format", "stream-json", "--verbose"}, extra...)
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("claude: %v\n%s\n%s", err, raw, stderr.String())
	}
	out := sessionOutput{raw: string(raw)}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Message struct {
				Content []struct {
					Type    string          `json:"type"`
					Name    string          `json:"name"`
					Content json.RawMessage `json:"content"`
					Text    string          `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "tool_use":
				out.tools = append(out.tools, c.Name)
			case "tool_result":
				out.texts = append(out.texts, string(c.Content))
			case "text":
				out.texts = append(out.texts, c.Text)
			}
		}
	}
	return out
}
