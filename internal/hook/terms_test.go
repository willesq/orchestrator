package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/spaces"
)

// termsSetup applies, in a fresh data directory, a policy where acme (org
// acme) and globex (org globex) each have a terms file, and returns a git
// checkout of acme/app with nothing unpushed.
func termsSetup(t *testing.T) string {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	terms := t.TempDir()
	for name, body := range map[string]string{"acme.txt": "falcon\n", "globex.txt": "zephyr\n"} {
		if err := os.WriteFile(filepath.Join(terms, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	e, err := spaces.Open(ctx, conn, spaces.Env{InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "acme", Kind: spaces.Isolated, TermsFile: filepath.Join(terms, "acme.txt")},
		{Name: "globex", Kind: spaces.Isolated, TermsFile: filepath.Join(terms, "globex.txt")},
	}}
	// Reason: binding orgs needs GitHub at apply; the checkout's org is
	// then in no space, which holds neither job's terms.
	r, ps, err := e.SaveDraft(ctx, p, "test", true)
	if err != nil || ps != nil {
		t.Fatal(err, ps)
	}
	if _, err := e.Apply(ctx, r.Number, "test", false, nil); err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, args := range [][]string{
		{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/app.git"},
		{"commit", "-q", "--allow-empty", "-m", "first"}, {"update-ref", "refs/remotes/origin/main", "HEAD"},
	} {
		cmd := osexec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x",
			"-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func termsPayload(t *testing.T, cmd, cwd string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Bash",
		"tool_input": map[string]string{"command": cmd}})
	return b
}

// TestOutboundTermsRefusedByHook: pre-tool-use refuses a commit that
// would send a term out, with a reason that names no term, and logs the
// event; other calls are untouched.
func TestOutboundTermsRefusedByHook(t *testing.T) {
	dir := termsSetup(t)
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("the Zephyr plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := osexec.Command("git", "-C", dir, "add", "n.md").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	c := &toolEventClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, bytes.NewReader(termsPayload(t, `git commit -m "notes"`, dir)), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	var d struct {
		HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil || d.HookSpecificOutput["permissionDecision"] != "deny" {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	reason := d.HookSpecificOutput["permissionDecisionReason"]
	if strings.Contains(strings.ToLower(reason), "zephyr") || !strings.Contains(reason, "outbound_term") || !strings.Contains(reason, `"globex"`) {
		t.Errorf("reason %q", reason)
	}
	if len(c.events) != 1 {
		t.Errorf("events %d, want the refused call logged", len(c.events))
	}
	// Ordinary calls print nothing.
	out.Reset()
	for _, cmd := range []string{"git status", "ls", `git commit -m "falcon"`} {
		if strings.Contains(cmd, "falcon") {
			// acme's term to an org in no space is refused too.
			continue
		}
		if err := run([]string{"pre-tool-use"}, bytes.NewReader(termsPayload(t, cmd, dir)), &out, factory(c, nil)); err != nil || out.Len() != 0 {
			t.Errorf("%q: printed %q, %v", cmd, out.String(), err)
		}
	}
}

// TestOutboundTermsSwitchedOff: with the terms layer switched off the
// check does not run.
func TestOutboundTermsSwitchedOff(t *testing.T) {
	dir := termsSetup(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	c, sw, _, err := spaces.NextSwitch(spaces.DefaultEnforcement(), spaces.LayerTerms, spaces.SwitchOff, "operator", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSwitch(ctx, conn, c, sw); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, bytes.NewReader(termsPayload(t, `git commit --allow-empty -m "zephyr"`, dir)), &out, factory(&toolEventClient{fakeClient: &fakeClient{}}, nil)); err != nil || out.Len() != 0 {
		t.Errorf("terms off: printed %q, %v", out.String(), err)
	}
}

// TestOutboundTermsUnreadablePolicy: an orch.db whose policy cannot be
// read refuses an outbound call, and only that.
func TestOutboundTermsUnreadablePolicy(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(os.Getenv("ORCH_DATA_DIR"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("ORCH_DATA_DIR"), "orch.db"), bytes.Repeat([]byte("junk "), 200), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := defaultCheckOutbound(ctx, Payload{ToolName: "Bash", Cwd: "/w", ToolInput: []byte(`{"command":"git push"}`)})
	if !r.Refuse || r.Rule != spaces.RuleOutboundUnchecked {
		t.Errorf("git push: %+v", r)
	}
	if r := defaultCheckOutbound(ctx, Payload{ToolName: "Bash", Cwd: "/w", ToolInput: []byte(`{"command":"ls"}`)}); r.Refuse {
		t.Errorf("ls: %+v", r)
	}
	// No orch.db at all: no policy, nothing checked.
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "none"))
	if r := defaultCheckOutbound(ctx, Payload{ToolName: "Bash", Cwd: "/w", ToolInput: []byte(`{"command":"git push"}`)}); r.Refuse {
		t.Errorf("no store: %+v", r)
	}
}
