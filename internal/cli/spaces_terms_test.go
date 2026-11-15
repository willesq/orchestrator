package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// rulesFake is what fakeClient's outbound rules RPCs answer and record.
var rulesFake struct {
	list   *orchdv1.OutboundRules
	change *orchdv1.ChangeOutboundRuleResponse
	err    error
	req    *orchdv1.ChangeOutboundRuleRequest
}

func (f *fakeClient) ListOutboundRules(context.Context, *orchdv1.ListOutboundRulesRequest, ...grpc.CallOption) (*orchdv1.OutboundRules, error) {
	return rulesFake.list, rulesFake.err
}

func (f *fakeClient) ChangeOutboundRule(_ context.Context, req *orchdv1.ChangeOutboundRuleRequest, _ ...grpc.CallOption) (*orchdv1.ChangeOutboundRuleResponse, error) {
	rulesFake.req = req
	return rulesFake.change, rulesFake.err
}

func resetRulesFake(t *testing.T) {
	t.Helper()
	reset := func() { rulesFake.list, rulesFake.change, rulesFake.err, rulesFake.req = nil, nil, nil, nil }
	reset()
	t.Cleanup(reset)
}

// TestSpacesTermsRulesList is SP-075.2.a1 in the CLI: 'orch spaces terms
// rules' lists the baseline and added rules with their state, the revision,
// who changed them and the history, and warns when the rules or the terms
// layer do not apply.
func TestSpacesTermsRulesList(t *testing.T) {
	resetRulesFake(t)
	at := time.Date(2026, 11, 14, 10, 5, 0, 0, time.Local)
	rulesFake.list = &orchdv1.OutboundRules{
		Rules: []*orchdv1.OutboundRule{
			{Id: "fail_closed_unreadable", Kind: "baseline", On: true, Summary: "refuse an outbound command that cannot be read in full", Baseline: true},
			{Id: "plain_variables", Kind: "baseline", On: false, Loosens: true, Summary: "read $PWD, $USER and $HOME", Baseline: true},
			{Id: "user-1", Kind: "fail_closed", On: true, Summary: "refuses a tool call whose tool name or text matches a pattern"},
		},
		Revision: 2, SetAt: at.Unix(), SetBy: "operator (process 9)", TermsLayer: "on",
		History: []*orchdv1.OutboundRuleChange{{Revision: 2, At: at.Unix(), By: "operator (process 9)", Action: "add", RuleId: "user-1"}},
		Kinds:   []string{"command", "destination", "fail_closed"},
	}
	for _, args := range [][]string{{"spaces", "terms", "rules"}, {"spaces", "terms", "rules", "list"}} {
		out, err := run(factory(&fakeClient{}, nil), "", args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"plain_variables", "baseline     off", "(loosens the strict reading)", "user-1",
			"fail_closed  on", "rules revision 2", "by operator (process 9)", "add     user-1", "<command|destination|fail_closed> (value on input)"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: output lacks %q:\n%s", args, want, out)
			}
		}
	}
	rulesFake.list = &orchdv1.OutboundRules{Problem: "the outbound rules in orch.db do not verify", TermsLayer: "off"}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "terms", "rules")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"warning: the outbound rules in orch.db do not verify", "the terms layer is off", "the baseline: no rule was ever changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	rulesFake.err = errors.New("orchd down")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "terms", "rules"); err == nil {
		t.Error("an RPC error was not returned")
	}
}

// TestSpacesTermsRulesChange is SP-075.2.a1 in the CLI: enable, disable,
// add and remove send one change each, report it by id only, and pass on
// orchd's refusal of a non-operator caller.
func TestSpacesTermsRulesChange(t *testing.T) {
	cases := []struct {
		args  []string
		stdin string
		want  *orchdv1.ChangeOutboundRuleRequest
		rep   *orchdv1.ChangeOutboundRuleResponse
		out   string
	}{
		{[]string{"enable", "plain_variables"}, "", &orchdv1.ChangeOutboundRuleRequest{Action: "enable", Id: "plain_variables"},
			&orchdv1.ChangeOutboundRuleResponse{RuleId: "plain_variables", Changed: true, Revision: 3}, "outbound rule plain_variables switched on (rules revision 3)"},
		{[]string{"disable", "user-1"}, "", &orchdv1.ChangeOutboundRuleRequest{Action: "disable", Id: "user-1"},
			&orchdv1.ChangeOutboundRuleResponse{RuleId: "user-1"}, "outbound rule user-1 was already switched off"},
		{[]string{"add", "fail_closed"}, "  re:deploy-[a-z]+\nignored\n", &orchdv1.ChangeOutboundRuleRequest{Action: "add", Kind: "fail_closed", Value: "re:deploy-[a-z]+"},
			&orchdv1.ChangeOutboundRuleResponse{RuleId: "user-2", Changed: true, Revision: 4}, "outbound rule user-2 added (rules revision 4)"},
		{[]string{"remove", "user-2"}, "", &orchdv1.ChangeOutboundRuleRequest{Action: "remove", Id: "user-2"},
			&orchdv1.ChangeOutboundRuleResponse{RuleId: "user-2", Changed: true, Revision: 5}, "outbound rule user-2 removed (rules revision 5)"},
	}
	for _, c := range cases {
		resetRulesFake(t)
		rulesFake.change = c.rep
		out, err := run(factory(&fakeClient{}, nil), c.stdin, append([]string{"spaces", "terms", "rules"}, c.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		r := rulesFake.req
		if r.GetAction() != c.want.GetAction() || r.GetId() != c.want.GetId() || r.GetKind() != c.want.GetKind() || r.GetValue() != c.want.GetValue() {
			t.Errorf("%v: request %+v", c.args, r)
		}
		if !strings.Contains(out, c.out) || strings.Contains(out, "deploy") {
			t.Errorf("%v: output %q", c.args, out)
		}
	}
	resetRulesFake(t)
	rulesFake.err = grpcstatus.Error(codes.PermissionDenied, "only the operator can change the outbound rules")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "terms", "rules", "disable", "plain_variables"); err == nil ||
		!strings.Contains(err.Error(), "only the operator") {
		t.Errorf("refusal: %v", err)
	}
	// The value never comes as an argument, and an empty input is refused
	// before orchd is asked.
	resetRulesFake(t)
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "terms", "rules", "add", "command", "glab"); err == nil {
		t.Error("add with the value as an argument ran")
	}
	if _, err := run(factory(&fakeClient{}, nil), "\n", "spaces", "terms", "rules", "add", "command"); err == nil ||
		!strings.Contains(err.Error(), "no rule value") || rulesFake.req != nil {
		t.Errorf("empty value: %v %+v", err, rulesFake.req)
	}
}
