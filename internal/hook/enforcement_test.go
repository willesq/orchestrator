package hook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// enforcementFake is what fakeClient's GetEnforcement answers.
var enforcementFake struct {
	rep   *orchdv1.EnforcementState
	err   error
	calls int
}

func (f *fakeClient) GetEnforcement(context.Context, *orchdv1.GetEnforcementRequest, ...grpc.CallOption) (*orchdv1.EnforcementState, error) {
	enforcementFake.calls++
	return enforcementFake.rep, enforcementFake.err
}

func (f *fakeClient) SetEnforcement(context.Context, *orchdv1.SetEnforcementRequest, ...grpc.CallOption) (*orchdv1.SetEnforcementResponse, error) {
	return nil, errors.New("hooks never switch enforcement")
}

func (f *fakeClient) SetSandboxSetting(context.Context, *orchdv1.SetSandboxSettingRequest, ...grpc.CallOption) (*orchdv1.SetSandboxSettingResponse, error) {
	return nil, errors.New("hooks never change the sandbox setting")
}

func setEnforcementFake(t *testing.T, rep *orchdv1.EnforcementState, err error) {
	t.Helper()
	enforcementFake.rep, enforcementFake.err, enforcementFake.calls = rep, err, 0
	t.Cleanup(func() { enforcementFake.rep, enforcementFake.err, enforcementFake.calls = nil, nil, 0 })
}

var since = time.Date(2026, 11, 11, 10, 5, 0, 0, time.UTC)

func relaxedState() *orchdv1.EnforcementState {
	return &orchdv1.EnforcementState{Switches: []*orchdv1.EnforcementSwitch{
		{Layer: "memory", Title: "memory filtering", State: "on", DefaultState: "on"},
		{Layer: "guard", Title: "tool guard", State: "off", DefaultState: "block-other-jobs", Relaxed: true,
			SetAt: since.Unix(), Until: since.Add(15 * time.Minute).Unix(), RevertsTo: "block-other-jobs"},
		{Layer: "hooks", Title: "tool hooks (pre-tool-use, cwd-changed)", State: "off", DefaultState: "on", Relaxed: true, SetAt: since.Unix()},
		{Layer: "terms", Title: "outbound terms\n## injected", State: "log", Relaxed: true},
	}}
}

// TestSessionStartNamesSwitchedOffLayers is the digest half of SP-070.2.a1
// (and SP-070.1.a4 for the tool hooks): every session's SessionStart digest,
// a queued task's included, names each layer that is off and since when.
func TestSessionStartNamesSwitchedOffLayers(t *testing.T) {
	for _, task := range []string{"", "task-1"} {
		t.Run("task="+task, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv(agentrun.TaskIDEnv, task)
			setEnforcementFake(t, relaxedState(), nil)
			var out bytes.Buffer
			if err := run([]string{"session-start"}, strings.NewReader(`{"session_id":"sid","source":"startup"}`), &out, factory(&fakeClient{}, nil)); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			for _, want := range []string{
				"## Orchestrator digest",
				"- [enforcement] orchd: guard (tool guard) is switched off since 2026-11-11 10:05 UTC; it reverts to block-other-jobs at 2026-11-11 10:20 UTC",
				"- [enforcement] orchd: hooks (tool hooks (pre-tool-use, cwd-changed)) is switched off since 2026-11-11 10:05 UTC",
				"terms (outbound terms ## injected) only logs (log)",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("digest lacks %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "memory") || strings.Count(got, "\n## ") != 0 {
				t.Errorf("digest names an enforcing layer or opens a section:\n%s", got)
			}
		})
	}
}

// TestSessionStartWithoutSwitchesOff: nothing is added while every layer
// enforces, or when orchd does not answer.
func TestSessionStartWithoutSwitchesOff(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	for name, cf := range map[string]clientFactory{
		"all on":      factory(&fakeClient{}, nil),
		"orchd down":  factory(nil, errors.New("down")),
		"rpc refused": factory(&fakeClient{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			rep := &orchdv1.EnforcementState{Switches: []*orchdv1.EnforcementSwitch{{Layer: "guard", State: "block-other-jobs"}}}
			var err error
			if name == "rpc refused" {
				rep, err = nil, errors.New("unavailable")
			}
			setEnforcementFake(t, rep, err)
			var out bytes.Buffer
			if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, cf); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Errorf("digest = %q, want none", out.String())
			}
		})
	}
}

// TestSessionStartNamesEnforcementProblems: a layer that does not enforce
// though its switch is on, and switches orch.db could not give orchd, are
// named too.
func TestSessionStartNamesEnforcementProblems(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	setEnforcementFake(t, &orchdv1.EnforcementState{
		Error: "the enforcement switches in orch.db were changed outside orchd; orchd keeps the ones it set",
		Switches: []*orchdv1.EnforcementSwitch{{Layer: "hooks", Title: "tool hooks", State: "on", Relaxed: true,
			Problem: "the switch is on but Claude Code's PreToolUse/orch-hook pre-tool-use is removed"}},
	}, nil)
	var out bytes.Buffer
	if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- [enforcement] orchd: hooks (tool hooks) does not enforce: the switch is on but Claude Code's PreToolUse/orch-hook pre-tool-use is removed",
		"- [enforcement] orchd: the enforcement switches in orch.db were changed outside orchd",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

// TestSessionStartReadsSwitchesWithoutOrchd: when orchd does not answer,
// the digest reads orch.db's switches itself, so stopping orchd cannot hide
// a layer the operator switched off; a store it cannot read is named.
func TestSessionStartReadsSwitchesWithoutOrchd(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cur := spaces.DefaultEnforcement()
	for _, set := range []struct {
		l     spaces.Layer
		state string
		dur   time.Duration
	}{{spaces.LayerHooks, "off", 0}, {spaces.LayerGuard, "off", time.Minute}, {spaces.LayerMemory, "off", time.Hour}} {
		// The guard's timer is up already; the memory switch's is not.
		at := time.Now().Add(-2 * time.Minute)
		c, sw, _, err := spaces.NextSwitch(cur, set.l, set.state, "operator (process 1)", set.dur, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := spaces.WriteSwitch(ctx, conn, c, sw); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	setEnforcementFake(t, nil, errors.New("unavailable"))
	var out bytes.Buffer
	if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "hooks (tool hooks (pre-tool-use, cwd-changed)) is switched off since") ||
		!strings.Contains(got, "memory (memory filtering) is switched off") ||
		!strings.Contains(got, "(orchd did not answer; read from orch.db)") || strings.Contains(got, "guard") {
		t.Errorf("digest without orchd:\n%s", got)
	}

	// A store that cannot be read.
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data2"))
	if err := os.MkdirAll(filepath.Dir(paths.DBFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.DBFile(), []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, factory(nil, errors.New("down"))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "could not be read, so a layer may be switched off") {
		t.Errorf("digest with an unreadable store:\n%s", out.String())
	}
}

// TestSessionStartIgnoresSandboxSetting is the digest half of SP-046.4.a2
// (INV-16 as amended by D-61): the sandbox setting off, or auto with
// nothing to deny, never marks a session's digest as enforcement off,
// whether orchd answers or the hook reads orch.db itself; a timed sandbox
// switch does. Since D-68 the digest names a changed setting once
// (TestSessionStartNamesSandboxSetting), as information, never as a layer
// that does not enforce.
func TestSessionStartIgnoresSandboxSetting(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	enforcementOff := func(digest string) bool {
		return strings.Contains(digest, "[enforcement]") || strings.Contains(digest, "does not enforce") || strings.Contains(digest, "switched off")
	}
	for _, sb := range []*orchdv1.SandboxSetting{
		{Mode: "off", DefaultMode: "auto", SetAt: since.Unix(), Detail: "off by choice"},
		{Mode: "auto", DefaultMode: "auto", Detail: "auto: no other isolated space's folders to deny"},
	} {
		setEnforcementFake(t, &orchdv1.EnforcementState{SandboxSetting: sb, Switches: []*orchdv1.EnforcementSwitch{
			{Layer: "sandbox", Title: "generated sandbox rules", State: "on", DefaultState: "on"},
		}}, nil)
		var out bytes.Buffer
		if err := dispatch("session-start", Payload{SessionID: "sid-" + sb.GetMode()}, &out, factory(&fakeClient{}, nil)); err != nil {
			t.Fatal(err)
		}
		if enforcementOff(out.String()) {
			t.Errorf("setting %s: digest marks enforcement off: %q", sb.GetMode(), out.String())
		}
		if sb.GetMode() == "auto" && out.Len() != 0 {
			t.Errorf("auto never set: digest = %q, want none", out.String())
		}
	}

	// orchd down: the hook reads orch.db, where the setting is off.
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, s, _, err := spaces.NextSandboxSetting(spaces.DefaultSandboxSetting(), spaces.SandboxOff, "operator (process 1)", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSandboxSetting(ctx, conn, c, s); err != nil {
		t.Fatal(err)
	}
	setEnforcementFake(t, nil, errors.New("unavailable"))
	var out bytes.Buffer
	if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	if enforcementOff(out.String()) {
		t.Errorf("setting off in orch.db: digest marks enforcement off: %q", out.String())
	}

	// A timed sandbox switch to off is named.
	cs, sw, _, err := spaces.NextSwitch(spaces.DefaultEnforcement(), spaces.LayerSandbox, spaces.SwitchOff, "operator (process 1)", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSwitch(ctx, conn, cs, sw); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	out.Reset()
	if err := dispatch("session-start", Payload{SessionID: "sid"}, &out, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sandbox (generated sandbox rules) is switched off since") {
		t.Errorf("timed switch: digest = %q", out.String())
	}
}

// pingFake is what fakeClient's Ping answers when set.
var pingFake *orchdv1.PingResponse

// TestSessionStartNamesSandboxSetting is D-68 (RC-216): after a change to
// the sandbox setting the next start digest of each session names it once;
// after an orchd start while it is off, again once; a setting never
// changed, and a restart while it is not off, are not named. Read from
// orch.db when orchd does not answer, it says so.
func TestSessionStartNamesSandboxSetting(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Cleanup(func() { pingFake = nil })
	state := func(mode string, setAt int64, uptime int64) {
		t.Helper()
		setEnforcementFake(t, &orchdv1.EnforcementState{SandboxSetting: &orchdv1.SandboxSetting{Mode: mode, DefaultMode: "auto", SetAt: setAt, Detail: "what " + mode + " does"}}, nil)
		pingFake = &orchdv1.PingResponse{UptimeSeconds: uptime}
	}
	digest := func(sid string) string {
		t.Helper()
		var out bytes.Buffer
		if err := dispatch("session-start", Payload{SessionID: sid}, &out, factory(&fakeClient{}, nil)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	named := func(d string) bool { return strings.Contains(d, "the sandbox setting is") }
	day := time.Hour * 24

	// Never set: auto is the default and is not named.
	state("auto", 0, 100)
	if d := digest("s1"); named(d) {
		t.Errorf("default setting named: %q", d)
	}
	// The operator sets it to on: each session's next digest names it once.
	state("on", since.Unix(), 100)
	for _, sid := range []string{"s1", "s2"} {
		if d := digest(sid); !named(d) || !strings.Contains(d, "is on (set 2026-11-11 10:05 UTC): what on does") {
			t.Errorf("%s after the change: %q", sid, d)
		}
		if d := digest(sid); named(d) {
			t.Errorf("%s named it twice: %q", sid, d)
		}
	}
	// A restart while on is not named again.
	state("on", since.Unix(), 1)
	if d := digest("s1"); named(d) {
		t.Errorf("restart while on named it: %q", d)
	}
	// Off: named once; again after orchd starts anew while it is off, and
	// not for the rounding of orchd's start between two calls.
	off := since.Add(day).Unix()
	state("off", off, 1000)
	if d := digest("s1"); !named(d) || !strings.Contains(d, "is off") || !strings.Contains(d, "orchd started") || strings.Contains(d, "[enforcement]") {
		t.Errorf("off: %q", d)
	}
	state("off", off, 1001)
	if d := digest("s1"); named(d) {
		t.Errorf("the same orchd named it again: %q", d)
	}
	state("off", off, 2)
	if d := digest("s1"); !named(d) {
		t.Errorf("a restart while off was not named: %q", d)
	}
	if d := digest("s1"); named(d) {
		t.Errorf("named twice after the restart: %q", d)
	}
	// Off with orchd's start unknown (no Ping answer) is named once.
	state("off", off, 0)
	pingFake = nil
	if d := digest("s3"); !named(d) || strings.Contains(d, "orchd started") {
		t.Errorf("off, start unknown: %q", d)
	}
	if d := digest("s3"); named(d) {
		t.Errorf("off, start unknown, named twice: %q", d)
	}

	// orchd down: read from orch.db, and said so; once per session.
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	c, s, _, err := spaces.NextSandboxSetting(spaces.DefaultSandboxSetting(), spaces.SandboxOff, "operator (process 1)", since)
	if err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteSandboxSetting(context.Background(), conn, c, s); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	setEnforcementFake(t, nil, errors.New("unavailable"))
	if d := digest("s4"); !named(d) || !strings.Contains(d, "read from orch.db") {
		t.Errorf("orchd down: %q", d)
	}
	if d := digest("s4"); named(d) {
		t.Errorf("orchd down, named twice: %q", d)
	}
	// A session ID that cannot name a state file is told every time.
	if d := digest("../bad"); !named(d) {
		t.Errorf("bad session ID: %q", d)
	}
}
