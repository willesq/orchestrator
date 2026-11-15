package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/hook/capture"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestMain keeps every hook test off the real data directory: the guard
// reads its files and orch.db from there.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "orch-hook-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
	_ = os.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "config"))
	// Reason: these tests exercise the hooks with memory spaces on; the
	// ones with spaces off (spaces_off_test.go) set it themselves.
	spacesOff = func() bool { return false }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// guardSetup writes guard files into a fresh data directory: an isolated
// space "job" at jobRoot, another job "other" at otherRoot (when set), the
// guard in mode, and a pin to "job" for each session in pinned.
func guardSetup(t *testing.T, mode, jobRoot, otherRoot string, pinned ...string) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	p := spaces.Policy{Spaces: []spaces.Space{{Name: "job", Kind: spaces.Isolated, Roots: []string{jobRoot}, GitHubOrgs: []string{"joborg"}}}}
	if otherRoot != "" {
		p.Spaces = append(p.Spaces, spaces.Space{Name: "other", Kind: spaces.Isolated, Roots: []string{otherRoot}, GitHubOrgs: []string{"otherorg"}})
	}
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 7, Policy: &p, Home: "/Users/user",
		Protection: spaces.Protection{Write: []string{paths.GuardDir()}}, Guard: spaces.GuardSwitch{State: mode}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range pinned {
		if err := spaces.WriteGuardPin(paths.GuardDir(), spaces.GuardPinFile{SessionID: id, Space: "job", Revision: 7}); err != nil {
			t.Fatal(err)
		}
	}
}

// denyOf parses a hook's deny output; ok is false when it printed none.
func denyOf(t *testing.T, out []byte) (reason string, ok bool) {
	t.Helper()
	if len(bytes.TrimSpace(out)) == 0 {
		return "", false
	}
	var d struct {
		HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("deny output %q: %v", out, err)
	}
	h := d.HookSpecificOutput
	if len(h) != 3 || h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || h["permissionDecisionReason"] == "" {
		t.Fatalf("deny output %q is not the PreToolUse deny format", out)
	}
	return h["permissionDecisionReason"], true
}

// TestGuardDenyFormat is SP-041.1.a1: a refused call prints the deny that
// SP-001 recorded both Claude Code and Codex honoring (the capture hook's
// output, see docs/internals/hooks.md), for a real PreToolUse payload of
// each harness. The reason names the rule, never the call's paths or text.
func TestGuardDenyFormat(t *testing.T) {
	var reference struct {
		HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(capture.DenyOutput()), &reference); err != nil {
		t.Fatal(err)
	}
	harnesses := map[string]bool{}
	for _, fx := range readFixtures(t) {
		if fx.event != "PreToolUse" || harnesses[fx.harness] || !strings.Contains(string(fx.payload), "/scratch/outside") {
			continue
		}
		harnesses[fx.harness] = true
		t.Run(fx.harness, func(t *testing.T) {
			var p Payload
			if err := json.Unmarshal(fx.payload, &p); err != nil {
				t.Fatal(err)
			}
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", p.SessionID)
			c := &toolEventClient{fakeClient: &fakeClient{}}
			var out bytes.Buffer
			if err := run([]string{"pre-tool-use"}, bytes.NewReader(fx.payload), &out, factory(c, nil)); err != nil {
				t.Fatal(err)
			}
			reason, ok := denyOf(t, out.Bytes())
			if !ok {
				t.Fatalf("%s %s: not refused", fx.harness, fx.scenario)
			}
			var got struct {
				HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
			}
			_ = json.Unmarshal(out.Bytes(), &got)
			for k, v := range reference.HookSpecificOutput {
				if _, ok := got.HookSpecificOutput[k]; !ok || (k != "permissionDecisionReason" && got.HookSpecificOutput[k] != v) {
					t.Errorf("field %s: got %q, the recorded format has %q", k, got.HookSpecificOutput[k], v)
				}
			}
			if !strings.Contains(reason, string(spaces.RuleOtherJobPath)) || strings.Contains(reason, "/scratch") || strings.Contains(reason, "file.txt") {
				t.Errorf("reason %q must name the rule and not the target", reason)
			}
			if len(c.events) != 1 || c.events[0].GuardVerdict != "refuse" || !c.events[0].GuardEnforced ||
				c.events[0].GuardRule != string(spaces.RuleOtherJobPath) || c.events[0].GuardPolicyRev != 7 {
				t.Errorf("event handed to orchd: %v", c.events)
			}
		})
	}
	if len(harnesses) != 2 {
		t.Fatalf("checked harnesses %v, want claude-code and codex", harnesses)
	}
}

// stallDecision makes every decision report mode and then wait past the
// guard's deadline, as a stalled file read or analysis would.
func stallDecision(t *testing.T, mode spaces.GuardMode) {
	t.Helper()
	old, oldTimeout := decideGuard, guardTimeout
	t.Cleanup(func() { decideGuard, guardTimeout = old, oldTimeout })
	guardTimeout = 20 * time.Millisecond
	decideGuard = func(ctx context.Context, _ spaces.GuardFiles, _ spaces.GuardInput, onMode func(spaces.GuardMode)) spaces.GuardResult {
		onMode(mode)
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		return spaces.GuardResult{}
	}
}

// TestGuardFailsClosed is SP-041.1.a2: a decision that times out, a
// session without a pin, and a policy that cannot be read each refuse in
// the enforcing modes and allow with the guard off or logging (INV-13).
func TestGuardFailsClosed(t *testing.T) {
	payload := `{"session_id":"sess","cwd":"/scratch/project","tool_name":"Bash","tool_input":{"command":"ls"}}`
	conditions := map[string]func(t *testing.T, mode string){
		"timeout": func(t *testing.T, mode string) {
			guardSetup(t, mode, "/scratch/project", "", "sess")
			stallDecision(t, spaces.GuardMode(mode))
		},
		"missing pin": func(t *testing.T, mode string) {
			guardSetup(t, mode, "/scratch/project", "")
		},
		"unreadable policy": func(t *testing.T, mode string) {
			guardSetup(t, mode, "/scratch/project", "", "sess")
			if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 7, Broken: true,
				Guard: spaces.GuardSwitch{State: mode}}); err != nil {
				t.Fatal(err)
			}
		},
	}
	rules := map[string]spaces.GuardRule{"timeout": spaces.RuleGuardTimeout, "missing pin": spaces.RulePinMissing,
		"unreadable policy": spaces.RulePolicyUnreadable}
	n := 0
	for name, setup := range conditions {
		for _, mode := range []string{spaces.SwitchOff, spaces.SwitchLog, spaces.SwitchBlockOtherJobs, spaces.SwitchStrict} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				setup(t, mode)
				c := &toolEventClient{fakeClient: &fakeClient{}}
				var out bytes.Buffer
				if err := run([]string{"pre-tool-use"}, strings.NewReader(payload), &out, factory(c, nil)); err != nil {
					t.Fatal(err)
				}
				_, refused := denyOf(t, out.Bytes())
				if want := spaces.GuardMode(mode).Enforcing(); refused != want {
					t.Errorf("refused = %v, want %v", refused, want)
				}
				if len(c.events) != 1 || c.events[0].GuardVerdict != "refuse" || c.events[0].GuardRule != string(rules[name]) ||
					c.events[0].GuardEnforced != refused {
					t.Errorf("event handed to orchd: %v", c.events)
				}
			})
			n++
		}
	}
	if n < 8 {
		t.Errorf("%d cases, want at least 8", n)
	}
}

// TestGuardTimeoutWithoutMode: a decision that stalls before it knows the
// guard's mode refuses, and a cwd-changed event is never refused.
func TestGuardTimeoutWithoutMode(t *testing.T) {
	guardSetup(t, spaces.SwitchOff, "/scratch/project", "", "sess")
	old, oldTimeout := decideGuard, guardTimeout
	t.Cleanup(func() { decideGuard, guardTimeout = old, oldTimeout })
	guardTimeout = 10 * time.Millisecond
	decideGuard = func(ctx context.Context, _ spaces.GuardFiles, _ spaces.GuardInput, _ func(spaces.GuardMode)) spaces.GuardResult {
		<-ctx.Done()
		return spaces.GuardResult{}
	}
	r := decideWithin(guardTimeout, spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: "sess"})
	if r.Verdict != spaces.Refuse || r.Rule != spaces.RuleGuardTimeout {
		t.Errorf("pre-tool-use: %s (%s)", r.Verdict, r.Rule)
	}
	r = decideWithin(guardTimeout, spaces.GuardInput{Kind: spaces.EventCwdChanged, SessionID: "sess"})
	if r.Verdict != spaces.Allow || r.Enforced {
		t.Errorf("cwd-changed: %s enforced=%v", r.Verdict, r.Enforced)
	}
	// A decision that panics refuses as well.
	decideGuard = func(context.Context, spaces.GuardFiles, spaces.GuardInput, func(spaces.GuardMode)) spaces.GuardResult {
		panic("boom")
	}
	if r := decideWithin(guardTimeout, spaces.GuardInput{Kind: spaces.EventPreToolUse}); r.Verdict != spaces.Refuse {
		t.Errorf("panic: %s", r.Verdict)
	}
}

// TestGuardCwdChangedNeverPrints: cwd-changed into another job is logged
// with the verdict the guard would give, and nothing is printed.
func TestGuardCwdChangedNeverPrints(t *testing.T) {
	guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside", "sess")
	c := &toolEventClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	in := `{"session_id":"sess","cwd":"/scratch/outside","old_cwd":"/scratch/project","new_cwd":"/scratch/outside"}`
	if err := run([]string{"cwd-changed"}, strings.NewReader(in), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || len(c.events) != 1 || c.events[0].GuardVerdict != "refuse" || c.events[0].GuardEnforced {
		t.Errorf("printed %q, events %v", out.String(), c.events)
	}
}

// TestGuardRefusalWaitsLongerForOrchd: a refused call's event gets the
// longer hand-off deadline; an allowed one the tool hooks' 20 ms.
func TestGuardRefusalWaitsLongerForOrchd(t *testing.T) {
	guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", "sess")
	for cmd, min := range map[string]time.Duration{"cat /scratch/outside/x": refusalEventTimeout, "ls": toolEventTimeout} {
		c := &toolEventClient{fakeClient: &fakeClient{}, block: true}
		in := `{"session_id":"sess","cwd":"/scratch/project","tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`
		start := time.Now()
		if err := run([]string{"pre-tool-use"}, strings.NewReader(in), &bytes.Buffer{}, factory(c, nil)); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took < min || took > min+refusalEventTimeout {
			t.Errorf("%s: took %v, want about %v", cmd, took, min)
		}
	}
}

// TestGuardReplaysFixtures is SP-041.2.a1: every recorded PreToolUse call
// of both harnesses, replayed under block-other-jobs with the session
// pinned to the space holding its project, is never refused for a target
// in that space. With /scratch/outside bound to another job, only calls
// that reach it are refused.
func TestGuardReplaysFixtures(t *testing.T) {
	layouts := []struct {
		name, job, other string
	}{
		{"every target in the session's space", "/scratch", ""},
		{"outside folder in no space", "/scratch/project", ""},
		{"outside folder in another job", "/scratch/project", "/scratch/outside"},
	}
	for _, l := range layouts {
		t.Run(l.name, func(t *testing.T) {
			var ids []string
			fixtures := readFixtures(t)
			for _, fx := range fixtures {
				var p Payload
				_ = json.Unmarshal(fx.payload, &p)
				ids = append(ids, p.SessionID)
			}
			guardSetup(t, spaces.SwitchBlockOtherJobs, l.job, l.other, ids...)
			calls, refused, ownRefused := 0, 0, 0
			for _, fx := range fixtures {
				if fx.event != "PreToolUse" {
					continue
				}
				calls++
				var out bytes.Buffer
				c := &toolEventClient{fakeClient: &fakeClient{}}
				if err := run([]string{"pre-tool-use"}, bytes.NewReader(fx.payload), &out, factory(c, nil)); err != nil {
					t.Fatal(err)
				}
				if _, ok := denyOf(t, out.Bytes()); !ok {
					continue
				}
				refused++
				reachesOther := l.other != "" && strings.Contains(string(fx.payload), l.other)
				if !reachesOther {
					ownRefused++
					t.Errorf("%s %s refused (%s) with every target in its own space or none", fx.harness, fx.scenario, c.events[0].GuardRule)
				}
			}
			t.Logf("%d calls replayed, %d refused, %d refused within the session's own space", calls, refused, ownRefused)
			if calls < 40 {
				t.Errorf("replayed %d calls, want every PreToolUse fixture", calls)
			}
		})
	}
}

// TestGuardDecisionLatency is SP-041.1.a3: the guard's decision, reading
// its snapshot and pin files and extracting targets, over the recorded
// payloads: p99 under 50 ms.
func TestGuardDecisionLatency(t *testing.T) {
	var inputs []spaces.GuardInput
	var ids []string
	for _, fx := range readFixtures(t) {
		if fx.event != "PreToolUse" {
			continue
		}
		var p Payload
		_ = json.Unmarshal(fx.payload, &p)
		inputs = append(inputs, guardInput("pre-tool-use", p))
		ids = append(ids, p.SessionID)
	}
	guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", ids...)
	const runs = 1000
	took := make([]time.Duration, 0, runs)
	for i := range runs {
		start := time.Now()
		decideWithin(guardTimeout, inputs[i%len(inputs)])
		took = append(took, time.Since(start))
	}
	sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
	p50, p99, worst := took[runs/2], took[runs*99/100], slices.Max(took)
	t.Logf("guard decision over %d runs: p50 %v, p99 %v, max %v", runs, p50, p99, worst)
	if p99 >= 50*time.Millisecond {
		t.Errorf("p99 %v, want < 50 ms", p99)
	}
}

// TestWriteDenyUnknownRule: a rule without a reason still gets a non-empty
// one, which Codex requires.
func TestWriteDenyUnknownRule(t *testing.T) {
	var out bytes.Buffer
	writeDeny(&out, "something_new", "")
	if reason, ok := denyOf(t, out.Bytes()); !ok || !strings.Contains(reason, "something_new") {
		t.Errorf("reason %q", reason)
	}
}

// failingReader fails every read, like a broken stdin pipe.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestGuardDecidesUnreadablePayload: a pre-tool-use payload that cannot be
// read is still decided: refused while the guard enforces (INV-13).
func TestGuardDecidesUnreadablePayload(t *testing.T) {
	for mode, want := range map[string]bool{spaces.SwitchBlockOtherJobs: true, spaces.SwitchLog: false} {
		guardSetup(t, mode, "/scratch/project", "", "sess")
		var out bytes.Buffer
		if err := run([]string{"pre-tool-use"}, failingReader{}, &out, factory(&toolEventClient{fakeClient: &fakeClient{}}, nil)); err != nil {
			t.Fatal(err)
		}
		if _, refused := denyOf(t, out.Bytes()); refused != want {
			t.Errorf("%s: refused = %v, want %v", mode, refused, want)
		}
		out.Reset()
		if err := run([]string{"stop"}, failingReader{}, &out, factory(nil, nil)); err != nil || out.Len() != 0 {
			t.Errorf("stop with unreadable stdin: %v %q", err, out.String())
		}
	}
	// A guard whose inputs cannot even be located refuses.
	old := guardFilesFor
	t.Cleanup(func() { guardFilesFor = old })
	guardFilesFor = func() spaces.GuardFiles { panic("no home") }
	if r := decideWithin(guardTimeout, spaces.GuardInput{Kind: spaces.EventPreToolUse}); r.Verdict != spaces.Refuse {
		t.Errorf("panicking inputs: %s", r.Verdict)
	}
}

// TestGuardInputCwdChanged: a cwd-changed event is judged running in its
// old directory and moving to its new one, as a command log replay builds
// it; without an old directory it runs in the payload's cwd.
func TestGuardInputCwdChanged(t *testing.T) {
	in := guardInput("cwd-changed", Payload{SessionID: "s", Cwd: "/n", OldCwd: "/o", NewCwd: "/n"})
	if in.Kind != spaces.EventCwdChanged || in.Cwd != "/o" || in.NewCwd != "/n" {
		t.Errorf("cwd-changed input = %+v", in)
	}
	if in := guardInput("cwd-changed", Payload{Cwd: "/n", NewCwd: "/n"}); in.Cwd != "/n" {
		t.Errorf("without old_cwd: %+v", in)
	}
	if in := guardInput("pre-tool-use", Payload{Cwd: "/c", OldCwd: "/o", ToolName: "Bash"}); in.Kind != spaces.EventPreToolUse || in.Cwd != "/c" {
		t.Errorf("pre-tool-use input = %+v", in)
	}
}
