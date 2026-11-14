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
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// termsPolicy is a policy where acme and globex each have a terms file
// (acme's term falcon, globex's zephyr) written under dir. Binding orgs
// needs GitHub at apply, so the checkouts' orgs are in no space, which
// holds neither space's terms.
func termsPolicy(t *testing.T, dir string) spaces.Policy {
	t.Helper()
	for name, body := range map[string]string{"acme.txt": "falcon\n", "globex.txt": "zephyr\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return spaces.Policy{Spaces: []spaces.Space{
		{Name: "acme", Kind: spaces.Isolated, TermsFile: filepath.Join(dir, "acme.txt")},
		{Name: "globex", Kind: spaces.Isolated, TermsFile: filepath.Join(dir, "globex.txt")},
	}}
}

// termsSnapshot writes the guard's snapshot of p (revision 7) with the
// guard in mode and the terms layer's switch terms, and pins session "s"
// to acme.
func termsSnapshot(t *testing.T, p spaces.Policy, mode string, terms spaces.GuardSwitch) {
	t.Helper()
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 7, Policy: &p, Home: "/Users/user",
		Protection: spaces.Protection{Write: []string{paths.GuardDir()}}, Guard: spaces.GuardSwitch{State: mode}, Terms: terms}); err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteGuardPin(paths.GuardDir(), spaces.GuardPinFile{SessionID: "s", Space: "acme", Revision: 7}); err != nil {
		t.Fatal(err)
	}
}

// termsSetup writes, in a fresh data directory, the guard's snapshot of
// termsPolicy with the guard in mode and the terms layer on, and returns a
// git checkout of acme/app with nothing unpushed.
func termsSetup(t *testing.T, mode string) string {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	termsSnapshot(t, termsPolicy(t, t.TempDir()), mode, spaces.GuardSwitch{})
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

// stageTerm stages a file holding globex's term in dir.
func stageTerm(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("the Zephyr plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := osexec.Command("git", "-C", dir, "add", "n.md").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
}

// preToolUse runs pre-tool-use for cmd in dir and returns the deny reason
// (ok false when none was printed) and the events handed to orchd.
func preToolUse(t *testing.T, cmd, dir string) (reason string, ok bool, c *toolEventClient) {
	t.Helper()
	c = &toolEventClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, bytes.NewReader(termsPayload(t, cmd, dir)), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(bytes.TrimSpace(out.Bytes()), []byte("\n")); n != 0 {
		t.Fatalf("%q: printed %d lines, want one deny at most: %q", cmd, n+1, out.String())
	}
	reason, ok = denyOf(t, out.Bytes())
	return reason, ok, c
}

// TestOutboundTermsRefusedByHook is RC-116 end to end in the hook: with
// the guard logging or enforcing, a commit that would send a term out is
// refused through the guard's one deny, whose reason names the rule and
// the term's space and not the term, and is handed to orchd as an enforced
// refusal with rule outbound_term and the snapshot's revision. Other calls
// are untouched.
func TestOutboundTermsRefusedByHook(t *testing.T) {
	for _, mode := range []string{spaces.SwitchLog, spaces.SwitchBlockOtherJobs, spaces.SwitchOff} {
		t.Run(mode, func(t *testing.T) {
			dir := termsSetup(t, mode)
			stageTerm(t, dir)
			reason, ok, c := preToolUse(t, `git commit -m "notes"`, dir)
			if !ok {
				t.Fatal("not refused")
			}
			if strings.Contains(strings.ToLower(reason), "zephyr") || !strings.Contains(reason, "outbound_term") ||
				!strings.Contains(reason, `"globex"`) || !strings.Contains(reason, "orch spaces log") {
				t.Errorf("reason %q", reason)
			}
			if len(c.events) != 1 {
				t.Fatalf("events %d, want the refused call logged", len(c.events))
			}
			ev := c.events[0]
			if ev.GuardVerdict != "refuse" || ev.GuardRule != string(spaces.RuleOutboundTerm) || !ev.GuardEnforced || ev.GuardPolicyRev != 7 {
				t.Errorf("event handed to orchd: verdict %q rule %q enforced %v rev %d", ev.GuardVerdict, ev.GuardRule, ev.GuardEnforced, ev.GuardPolicyRev)
			}
			for _, cmd := range []string{"git status", "ls", "git log --oneline"} {
				if r, ok, c := preToolUse(t, cmd, dir); ok {
					t.Errorf("%q refused: %q", cmd, r)
				} else if len(c.events) != 1 || c.events[0].GuardRule == string(spaces.RuleOutboundTerm) {
					t.Errorf("%q: events %v", cmd, c.events)
				}
			}
		})
	}
}

// TestOutboundTermsUncheckedRecorded: a command the check cannot read is
// refused as outbound_unchecked, through the same deny and record.
func TestOutboundTermsUncheckedRecorded(t *testing.T) {
	dir := termsSetup(t, spaces.SwitchLog)
	reason, ok, c := preToolUse(t, `git commit -m "$(date)"`, dir)
	if !ok || !strings.Contains(reason, "outbound_unchecked") {
		t.Fatalf("reason %q, refused %v", reason, ok)
	}
	if ev := c.events[0]; ev.GuardVerdict != "refuse" || ev.GuardRule != string(spaces.RuleOutboundUnchecked) || !ev.GuardEnforced {
		t.Errorf("event: %v", ev)
	}
}

// TestOutboundTermsAfterGuard: a call the enforcing guard refuses gets
// the guard's deny alone; the terms check does not run for it.
func TestOutboundTermsAfterGuard(t *testing.T) {
	dir := termsSetup(t, spaces.SwitchBlockOtherJobs)
	stageTerm(t, dir)
	called := false
	old := checkOutbound
	t.Cleanup(func() { checkOutbound = old })
	checkOutbound = func(ctx context.Context, p Payload, g spaces.GuardTerms) spaces.OutboundResult {
		called = true
		return old(ctx, p, g)
	}
	// Session "x" holds no pin: the guard refuses it as pin_missing.
	b, _ := json.Marshal(map[string]any{"session_id": "x", "cwd": dir, "tool_name": "Bash",
		"tool_input": map[string]string{"command": `git commit -m "notes"`}})
	c := &toolEventClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, bytes.NewReader(b), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	reason, ok := denyOf(t, out.Bytes())
	if !ok || !strings.Contains(reason, string(spaces.RulePinMissing)) || called {
		t.Errorf("reason %q, terms checked %v", reason, called)
	}
	if c.events[0].GuardRule != string(spaces.RulePinMissing) {
		t.Errorf("rule %q", c.events[0].GuardRule)
	}
}

// TestOutboundTermsSwitchFromSnapshot: the check reads the terms layer's
// switch from the guard's snapshot (RC-116), not orch.db: off there, it
// does not run; a timed off switch whose time is up is on again; a
// snapshot without the switch holds the default, on.
func TestOutboundTermsSwitchFromSnapshot(t *testing.T) {
	dir := termsSetup(t, spaces.SwitchLog)
	p := termsPolicy(t, t.TempDir())
	cmd := `git commit --allow-empty -m "zephyr"`
	for _, tc := range []struct {
		name   string
		sw     spaces.GuardSwitch
		refuse bool
	}{
		{"off", spaces.GuardSwitch{State: spaces.SwitchOff}, false},
		{"off for an hour", spaces.GuardSwitch{State: spaces.SwitchOff, Until: time.Now().Add(time.Hour), Previous: spaces.SwitchOn}, false},
		{"off until a minute ago", spaces.GuardSwitch{State: spaces.SwitchOff, Until: time.Now().Add(-time.Minute), Previous: spaces.SwitchOn}, true},
		{"on", spaces.GuardSwitch{State: spaces.SwitchOn}, true},
		{"absent", spaces.GuardSwitch{}, true},
	} {
		termsSnapshot(t, p, spaces.SwitchLog, tc.sw)
		if _, ok, _ := preToolUse(t, cmd, dir); ok != tc.refuse {
			t.Errorf("%s: refused %v, want %v", tc.name, ok, tc.refuse)
		}
	}
	// An orch.db that says off does not switch the check off while the
	// snapshot says on.
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	ch, sw, _, err := spaces.NextSwitch(spaces.DefaultEnforcement(), spaces.LayerTerms, spaces.SwitchOff, "operator", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSwitch(ctx, conn, ch, sw); err != nil {
		t.Fatal(err)
	}
	termsSnapshot(t, p, spaces.SwitchLog, spaces.GuardSwitch{State: spaces.SwitchOn})
	if _, ok, _ := preToolUse(t, cmd, dir); !ok {
		t.Error("orch.db's switch was read instead of the snapshot's")
	}
}

// TestOutboundTermsUnreadablePolicy: while the snapshot cannot be read
// and a policy was applied, an outbound call is refused (the terms cannot
// be known) unless orch.db says the terms layer is off; other calls are
// not. With nothing ever applied nothing is checked.
func TestOutboundTermsUnreadablePolicy(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	g := func() spaces.GuardTerms {
		return decideWithin(time.Second, spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: "s", Tool: "Bash"}).Terms
	}
	push := Payload{ToolName: "Bash", Cwd: "/w", ToolInput: []byte(`{"command":"git push"}`)}
	ls := Payload{ToolName: "Bash", Cwd: "/w", ToolInput: []byte(`{"command":"ls"}`)}
	// No orch.db, no snapshot: no policy, nothing checked.
	if r := defaultCheckOutbound(ctx, push, g()); r.Refuse {
		t.Errorf("no store: %+v", r)
	}
	// A policy applied, the guard logging, the snapshot gone.
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	e, err := spaces.Open(ctx, conn, spaces.Env{InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	r, ps, err := e.SaveDraft(ctx, termsPolicy(t, t.TempDir()), "test", true)
	if err != nil || ps != nil {
		t.Fatal(err, ps)
	}
	if _, err := e.Apply(ctx, r.Number, "test", false, nil); err != nil {
		t.Fatal(err)
	}
	if res := defaultCheckOutbound(ctx, push, g()); !res.Refuse || res.Rule != spaces.RuleOutboundUnchecked {
		t.Errorf("git push: %+v", res)
	}
	if res := defaultCheckOutbound(ctx, ls, g()); res.Refuse {
		t.Errorf("ls: %+v", res)
	}
	ch, sw, _, err := spaces.NextSwitch(spaces.DefaultEnforcement(), spaces.LayerTerms, spaces.SwitchOff, "operator", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSwitch(ctx, conn, ch, sw); err != nil {
		t.Fatal(err)
	}
	if res := defaultCheckOutbound(ctx, push, g()); res.Refuse {
		t.Errorf("terms off in orch.db: %+v", res)
	}
}

// TestOutboundTermsPanicFailsClosed: a check that panics refuses the call.
func TestOutboundTermsPanicFailsClosed(t *testing.T) {
	old := checkOutbound
	t.Cleanup(func() { checkOutbound = old })
	checkOutbound = func(context.Context, Payload, spaces.GuardTerms) spaces.OutboundResult { panic("boom") }
	if r := outboundTerms(Payload{}, spaces.GuardTerms{}); !r.Refuse || r.Rule != spaces.RuleOutboundUnchecked {
		t.Errorf("%+v", r)
	}
}
