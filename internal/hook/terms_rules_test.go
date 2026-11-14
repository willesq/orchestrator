package hook

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// rulesSnapshot rewrites the guard's snapshot of termsPolicy, guard logging,
// with the operator's outbound rules (SP-075).
func rulesSnapshot(t *testing.T, rules *spaces.OutboundRules, broken bool) {
	t.Helper()
	p := termsPolicy(t, t.TempDir())
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 7, Policy: &p, Home: "/Users/user",
		Protection: spaces.Protection{Write: []string{paths.GuardDir()}}, Guard: spaces.GuardSwitch{State: spaces.SwitchLog},
		Rules: rules, RulesRev: 3, RulesBroken: broken}); err != nil {
		t.Fatal(err)
	}
}

// TestOutboundRulesReachHook is SP-075.1.a3 in the hook: the rules in the
// guard snapshot decide the hook's terms check. A baseline rule switched off
// and an added rule both apply, the refusal names the rule by id only, and
// rules orchd could not read refuse outbound calls.
func TestOutboundRulesReachHook(t *testing.T) {
	dir := termsSetup(t, spaces.SwitchLog)
	rs, _, err := spaces.OutboundRules{}.Add("fail_closed", "deploy-prod")
	if err != nil {
		t.Fatal(err)
	}
	rs, _, _ = rs.Switch(string(spaces.OutboundPlainVariables), false)
	rulesSnapshot(t, &rs, false)

	reason, ok, _ := preToolUse(t, "./deploy-prod --now", dir)
	if !ok || !strings.Contains(reason, "outbound_unchecked") || !strings.Contains(reason, "user-1") ||
		strings.Contains(reason, "deploy-prod") {
		t.Errorf("fail_closed rule: %v %q", ok, reason)
	}
	if _, ok, _ := preToolUse(t, `git -C "$PWD" commit --allow-empty -m x`, dir); !ok {
		t.Error("plain_variables off: $PWD was read")
	}
	if _, ok, _ := preToolUse(t, "ls", dir); ok {
		t.Error("ls refused")
	}
	// The baseline reads $PWD.
	rulesSnapshot(t, nil, false)
	for _, cmd := range []string{`git -C "$PWD" commit --allow-empty -m x`, "./deploy-prod --now"} {
		if reason, ok, _ := preToolUse(t, cmd, dir); ok {
			t.Errorf("baseline %q: refused: %s", cmd, reason)
		}
	}
	// Broken rules: no call they may have covered can be vouched for,
	// so every shell command and MCP tool is refused; a local file tool
	// sends nothing out.
	rulesSnapshot(t, nil, true)
	for _, cmd := range []string{"git push", "ls"} {
		if reason, ok, _ := preToolUse(t, cmd, dir); !ok || !strings.Contains(reason, "cannot be read") {
			t.Errorf("broken rules: %q: %v %q", cmd, ok, reason)
		}
	}
	g := decideWithin(time.Second, spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: "s", Tool: "Read", Cwd: dir}).Terms
	if !g.RulesBroken {
		t.Fatalf("snapshot's broken rules not read: %+v", g)
	}
	ctx := context.Background()
	mcp := Payload{ToolName: "mcp__github__create_issue", Cwd: dir, ToolInput: []byte(`{"title":"x"}`)}
	read := Payload{ToolName: "Read", Cwd: dir, ToolInput: []byte(`{"file_path":"/x"}`)}
	if r := defaultCheckOutbound(ctx, mcp, g); !r.Refuse {
		t.Error("broken rules: MCP tool allowed")
	}
	if r := defaultCheckOutbound(ctx, read, g); r.Refuse {
		t.Errorf("broken rules: Read refused: %+v", r)
	}
	// A rule set that does not validate refuses an outbound call too.
	bad := &spaces.OutboundRules{Off: []spaces.OutboundRule{"nope"}}
	g = spaces.GuardTerms{Known: true, State: spaces.SwitchOn, Rules: bad}
	p := termsPolicy(t, t.TempDir())
	g.Policy, _ = spaces.Validate(p, spaces.Env{InGitTree: func(string) bool { return false }})
	push := Payload{ToolName: "Bash", Cwd: dir, ToolInput: []byte(`{"command":"git push"}`)}
	if r := defaultCheckOutbound(ctx, push, g); !r.Refuse || r.Rule != spaces.RuleOutboundUnchecked {
		t.Errorf("invalid rules: %+v", r)
	}
}
