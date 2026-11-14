package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// enforceFake is what fakeClient's GetEnforcement and SetEnforcement answer
// and record.
var enforceFake struct {
	get        *orchdv1.EnforcementState
	set        *orchdv1.SetEnforcementResponse
	err        error
	setReq     *orchdv1.SetEnforcementRequest
	sandbox    *orchdv1.SetSandboxSettingResponse
	sandboxReq *orchdv1.SetSandboxSettingRequest
}

func (f *fakeClient) GetEnforcement(context.Context, *orchdv1.GetEnforcementRequest, ...grpc.CallOption) (*orchdv1.EnforcementState, error) {
	return enforceFake.get, enforceFake.err
}

func (f *fakeClient) SetEnforcement(_ context.Context, req *orchdv1.SetEnforcementRequest, _ ...grpc.CallOption) (*orchdv1.SetEnforcementResponse, error) {
	enforceFake.setReq = req
	return enforceFake.set, enforceFake.err
}

func (f *fakeClient) SetSandboxSetting(_ context.Context, req *orchdv1.SetSandboxSettingRequest, _ ...grpc.CallOption) (*orchdv1.SetSandboxSettingResponse, error) {
	enforceFake.sandboxReq = req
	return enforceFake.sandbox, enforceFake.err
}

func resetEnforceFake(t *testing.T) {
	t.Helper()
	reset := func() {
		enforceFake.get, enforceFake.set, enforceFake.err, enforceFake.setReq = nil, nil, nil, nil
		enforceFake.sandbox, enforceFake.sandboxReq = nil, nil
	}
	reset()
	t.Cleanup(reset)
}

func TestSpacesEnforceList(t *testing.T) {
	resetEnforceFake(t)
	at := time.Date(2026, 11, 11, 10, 5, 0, 0, time.Local)
	enforceFake.get = &orchdv1.EnforcementState{Error: "could not read", Switches: []*orchdv1.EnforcementSwitch{
		{Layer: "memory", Title: "memory filtering", State: "on", DefaultState: "on"},
		{Layer: "hooks", Title: "tool hooks", State: "on", DefaultState: "on", Relaxed: true, Problem: "PreToolUse is removed"},
		{Layer: "guard", Title: "tool guard", State: "off", DefaultState: "block-other-jobs", Relaxed: true,
			SetAt: at.Unix(), SetBy: "operator (process 9)", Until: at.Add(15 * time.Minute).Unix(), RevertsTo: "block-other-jobs"},
	}}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"warning: could not read", "LAYER", "memory  ", "guard", "2026-11-11 10:05",
		"NOT ENFORCING", "NOT ENFORCING: PreToolUse is removed", "until 10:20 Nov 11, then block-other-jobs", "set by operator (process 9)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce", "--for", "1m"); err == nil {
		t.Error("--for without a layer was accepted")
	}
	enforceFake.err = errors.New("orchd down")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce"); err == nil {
		t.Error("an RPC error was swallowed")
	}
}

func TestSpacesEnforceSet(t *testing.T) {
	resetEnforceFake(t)
	at := time.Date(2026, 11, 11, 10, 5, 0, 0, time.Local)
	enforceFake.set = &orchdv1.SetEnforcementResponse{Changed: true, Notes: []string{"Claude Code hook X is not installed"},
		Current: &orchdv1.EnforcementSwitch{Layer: "hooks", State: "off", Until: at.Unix(), RevertsTo: "on"}}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce", "hooks", "off", "--for", "15m")
	if err != nil {
		t.Fatal(err)
	}
	if r := enforceFake.setReq; r.GetLayer() != "hooks" || r.GetState() != "off" || r.GetForSeconds() != 900 {
		t.Errorf("request = %v", r)
	}
	if !strings.Contains(out, "hooks is now off until 10:05 Nov 11, then on") || !strings.Contains(out, "note: Claude Code hook X is not installed") {
		t.Errorf("output:\n%s", out)
	}
	enforceFake.set = &orchdv1.SetEnforcementResponse{Current: &orchdv1.EnforcementSwitch{Layer: "guard", State: "strict"}}
	if out, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce", "guard", "strict"); err != nil || !strings.Contains(out, "guard is already strict") {
		t.Errorf("no-op: %q %v", out, err)
	}
	for _, args := range [][]string{{"guard"}, {"guard", "off", "extra"}, {"guard", "off", "--for", "500ms"}, {"guard", "off", "--for", "-1m"}} {
		if _, err := run(factory(&fakeClient{}, nil), "", append([]string{"spaces", "enforce"}, args...)...); err == nil {
			t.Errorf("enforce %v accepted", args)
		}
	}
	enforceFake.err = errors.New("only the operator can switch enforcement")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "enforce", "guard", "off"); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Errorf("refusal = %v", err)
	}
}

func TestEventDetailShowsRule(t *testing.T) {
	if d := eventDetail(&orchdv1.ToolEvent{Command: "ls", Decision: "not enforced", Rule: "would refuse (other-job)"}); d != "ls [would refuse (other-job)]" {
		t.Errorf("detail = %q", d)
	}
	if d := eventDetail(&orchdv1.ToolEvent{Event: "cwd-changed", OldCwd: "/a", NewCwd: "/b"}); d != "/a -> /b" {
		t.Errorf("detail = %q", d)
	}
}

// TestSpacesSandboxCmd (SP-046.4.a1): 'orch spaces sandbox' shows the
// setting and what it does; with a value it asks orchd to set it (orchd
// refuses anyone but the operator) and says whether it changed.
func TestSpacesSandboxCmd(t *testing.T) {
	resetEnforceFake(t)
	at := time.Date(2026, 11, 14, 9, 30, 0, 0, time.Local)
	enforceFake.get = &orchdv1.EnforcementState{SandboxSetting: &orchdv1.SandboxSetting{
		Mode: "off", DefaultMode: "auto", SetAt: at.Unix(), SetBy: "operator (process 9)",
		Detail: "off by choice", Problem: "changed outside orchd",
	}}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sandbox setting: off; set 2026-11-14 09:30 by operator (process 9)", "  off by choice", "warning: changed outside orchd"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = run(factory(&fakeClient{}, nil), "", "spaces", "enforce")
	if err != nil || !strings.Contains(out, "sandbox setting: off") {
		t.Errorf("enforce listing: %v\n%s", err, out)
	}
	enforceFake.get = &orchdv1.EnforcementState{SandboxSetting: &orchdv1.SandboxSetting{Mode: "auto", DefaultMode: "auto"}}
	if out, _ := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox"); !strings.Contains(out, "sandbox setting: auto (default)") {
		t.Errorf("default:\n%s", out)
	}
	enforceFake.get = &orchdv1.EnforcementState{}
	if out, _ := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox"); !strings.Contains(out, "unknown") {
		t.Errorf("no setting:\n%s", out)
	}

	enforceFake.sandbox = &orchdv1.SetSandboxSettingResponse{Changed: true, Current: &orchdv1.SandboxSetting{Mode: "on", Detail: "on: every space's sessions get orch's sandbox rules"}}
	out, err = run(factory(&fakeClient{}, nil), "", "spaces", "sandbox", "on")
	if err != nil || enforceFake.sandboxReq.GetMode() != "on" || !strings.Contains(out, "the sandbox setting is now on") || !strings.Contains(out, "every space") {
		t.Errorf("set: %v %+v\n%s", err, enforceFake.sandboxReq, out)
	}
	enforceFake.sandbox = &orchdv1.SetSandboxSettingResponse{Current: &orchdv1.SandboxSetting{Mode: "on"}}
	if out, _ := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox", "on"); !strings.Contains(out, "already on") {
		t.Errorf("unchanged:\n%s", out)
	}
	enforceFake.err = errors.New("only the operator can change the sandbox setting")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox", "off"); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Errorf("refused: %v", err)
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox"); err == nil {
		t.Error("a failed read was not reported")
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "sandbox", "on", "off"); err == nil {
		t.Error("two values accepted")
	}
}
