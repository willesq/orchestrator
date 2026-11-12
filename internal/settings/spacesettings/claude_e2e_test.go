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
// With ORCH_E2E_GH_PROFILE set to a gh profile folder the user logged in to
// (GH_CONFIG_DIR=<it> gh auth login), it is job A's gh_config_dir and the
// session's 'gh auth status' must name that profile's account.
//
// With ORCH_E2E_LAYOUT set to a folder, the per-space folders live there,
// so job A's Claude Code profile (<it>/joba/claude) outlives the run; once
// the user has logged in to it (CLAUDE_CONFIG_DIR=<it>/joba/claude claude,
// then /login), the launcher probes (SP-046.2.a2) start sessions the way
// 'orch run joba -- claude' does, from a folder outside every root, and
// expect the same denies as inside the checkout.
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
	ghProfile := os.Getenv("ORCH_E2E_GH_PROFILE")
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{codeA}, GHConfigDir: ghProfile},
		{Name: "jobb", Kind: spaces.Isolated, Roots: []string{codeB}},
	}}
	res, err := spaces.Validate(p, spaces.Env{Home: base, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	layout := Layout{Dir: filepath.Join(base, "data", "spaces")}
	if d := os.Getenv("ORCH_E2E_LAYOUT"); d != "" {
		layout = Layout{Dir: d}
	}
	checkout := gitRepo(t, filepath.Join(codeA, "app"))
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
	if rep := s.Sync(context.Background(), res, true); len(rep.Written) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("sync: %+v", rep)
	}
	if err := EnsurePrivateDir(layout.TmpDir("jobb")); err != nil {
		t.Fatal(err)
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
	runProbes := func(t *testing.T, dir string, env ...string) {
		for _, pr := range probes {
			t.Run(pr.name, func(t *testing.T) {
				probe(t, claude, dir, pr.prompt, pr.tool, pr.leak, pr.control, env...)
			})
		}
	}
	runProbes(t, deep)
	if os.Getenv("ORCH_E2E_LAYOUT") != "" {
		t.Run("launcher", func(t *testing.T) {
			userDir := os.Getenv("CLAUDE_CONFIG_DIR")
			if userDir == "" {
				home, _ := os.UserHomeDir()
				userDir = filepath.Join(home, ".claude")
			}
			r := For(res, "joba", layout)
			profile, err := WriteClaudeProfile(layout, "joba", userDir, ClaudeFragment(r))
			if err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(base, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatal(err)
			}
			env := []string{"CLAUDE_CONFIG_DIR=" + profile}
			for k, v := range r.Env() {
				env = append(env, k+"="+v)
			}
			runProbes(t, scratch, env...)
		})
	}
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
		if !strings.Contains(out.results(), "TMPDIR="+layout.TmpDir("joba")) {
			t.Errorf("the session's TMPDIR is not job A's (%s):\n%s", layout.TmpDir("joba"), out.results())
		}
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
	if ghProfile != "" {
		t.Run("gh", func(t *testing.T) {
			want := ghAccount(t, ghProfile)
			out := runClaude(t, claude, deep, "Run exactly this one Bash command and show its output: gh auth status. Do not use any other tool. Then reply DONE.")
			if !strings.Contains(out.results(), want) {
				t.Errorf("gh auth status in the session does not name %s:\n%s", want, out.results())
			}
		})
	}
}

// probe asks a session in dir to call tool on job B's folder and on a
// control in job A's, and checks what came back.
func probe(t *testing.T, claude, dir, prompt, tool, leak, control string, env ...string) {
	t.Helper()
	out := runClaude(t, claude, dir, prompt+" Do not use any other tool. Then reply DONE.", env...)
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

// ghAccount is the account the gh profile is logged in to, as gh prints it.
func ghAccount(t *testing.T, profile string) string {
	t.Helper()
	cmd := exec.Command("gh", "api", "user", "--jq", ".login")
	cmd.Env = append(os.Environ(), "GH_CONFIG_DIR="+profile)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gh profile %s is not logged in: %v", profile, err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p", prompt, "--model", "haiku", "--permission-mode", "default",
		"--allowedTools", "Read Grep Glob Edit Write Bash", "--output-format", "stream-json", "--verbose")
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
