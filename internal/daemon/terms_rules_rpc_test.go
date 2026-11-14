package daemon

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestOutboundRulesRPCs is SP-075.2.a1 and a2 through the RPCs the CLI and
// the app use: ListOutboundRules lists every rule by id, kind and state to
// any caller (who changed them and the history only to the operator), and
// ChangeOutboundRule is operator-only; no response holds a rule's value.
func TestOutboundRulesRPCs(t *testing.T) {
	s := spacesServer(t)
	withEnforcement(t, s)
	withTermsRules(t, s)
	list := func(ctx context.Context) *orchdv1.OutboundRules {
		t.Helper()
		r, err := s.ListOutboundRules(ctx, &orchdv1.ListOutboundRulesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	base := list(operatorCtx)
	if len(base.GetRules()) != len(spaces.OutboundBaseline) || base.GetRevision() != 0 || base.GetTermsLayer() != "on" ||
		strings.Join(base.GetKinds(), ",") != "command,destination,fail_closed" {
		t.Fatalf("baseline list: %v", base)
	}
	for _, r := range base.GetRules() {
		if !r.GetOn() || !r.GetBaseline() || r.GetKind() != "baseline" || r.GetSummary() == "" {
			t.Errorf("baseline rule: %v", r)
		}
	}
	session := callerCtx(callerAuth{pid: 7, harness: "claude"})
	for _, req := range []*orchdv1.ChangeOutboundRuleRequest{
		{Action: "disable", Id: "plain_variables"}, {Action: "add", Kind: "command", Value: "glab"}, {Action: "rename", Id: "x"},
	} {
		if _, err := s.ChangeOutboundRule(session, req); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("session %v: %v", req, err)
		}
	}
	for _, c := range []struct {
		req  *orchdv1.ChangeOutboundRuleRequest
		code codes.Code
	}{
		{&orchdv1.ChangeOutboundRuleRequest{Action: "rename", Id: "x"}, codes.InvalidArgument},
		{&orchdv1.ChangeOutboundRuleRequest{Action: "disable"}, codes.InvalidArgument},
		{&orchdv1.ChangeOutboundRuleRequest{Action: "add", Kind: "allow", Value: "glab"}, codes.InvalidArgument},
	} {
		if _, err := s.ChangeOutboundRule(operatorCtx, c.req); grpcstatus.Code(err) != c.code {
			t.Errorf("%v: %v, want %s", c.req, err, c.code)
		}
	}
	rep, err := s.ChangeOutboundRule(operatorCtx, &orchdv1.ChangeOutboundRuleRequest{Action: "add", Kind: "destination", Value: "acme/public-sdk"})
	if err != nil || !rep.GetChanged() || rep.GetRuleId() != "user-1" || rep.GetRevision() != 1 {
		t.Fatalf("add: %v %v", rep, err)
	}
	if _, err := s.ChangeOutboundRule(operatorCtx, &orchdv1.ChangeOutboundRuleRequest{Action: "disable", Id: "plain_variables"}); err != nil {
		t.Fatal(err)
	}
	op := list(operatorCtx)
	if op.GetRevision() != 2 || op.GetSetBy() != "operator (process 42)" || len(op.GetHistory()) != 2 ||
		op.GetHistory()[0].GetRuleId() != "plain_variables" || len(op.GetRules()) != len(spaces.OutboundBaseline)+1 {
		t.Fatalf("operator list: %v", op)
	}
	if u := op.GetRules()[len(op.GetRules())-1]; u.GetId() != "user-1" || u.GetKind() != "destination" || !u.GetOn() {
		t.Errorf("added rule: %v", u)
	}
	sess := list(session)
	if sess.GetSetBy() != "" || len(sess.GetHistory()) != 0 || len(sess.GetRules()) != len(op.GetRules()) {
		t.Errorf("session list: %v", sess)
	}
	for _, m := range []string{op.String(), sess.String(), rep.String()} {
		assertNoRuleValues(t, "response", m)
	}
	// Rules that cannot be read list as the baseline, with the problem.
	s.termsRules.mu.Lock()
	s.termsRules.problem = "the outbound rules in orch.db do not verify"
	s.termsRules.mu.Unlock()
	if b := list(operatorCtx); b.GetProblem() == "" || len(b.GetRules()) != len(spaces.OutboundBaseline) {
		t.Errorf("broken list: %v", b)
	}
	// Without a store: the baseline, and no change.
	bare := &server{}
	if r, err := bare.ListOutboundRules(operatorCtx, &orchdv1.ListOutboundRulesRequest{}); err != nil || len(r.GetRules()) != len(spaces.OutboundBaseline) {
		t.Errorf("no store list: %v %v", r, err)
	}
}

// TestRecordedRulesRev is D-66 at receipt: the revision the hook names is
// recorded when orchd has applied it (0 up to its own), else orchd's own.
func TestRecordedRulesRev(t *testing.T) {
	r := &termsRules{cur: spaces.StoredRules{Rev: 4}}
	rev := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		claimed *int64
		want    int64
	}{
		{nil, 4}, {rev(0), 0}, {rev(3), 3}, {rev(4), 4}, {rev(5), 4}, {rev(-2), 4}, {rev(-1), unreadableRulesRev},
	} {
		if got := r.recordedRulesRev(c.claimed); got != c.want {
			t.Errorf("claimed %v: %d, want %d", c.claimed, got, c.want)
		}
	}
	broken := &termsRules{problem: "unreadable"}
	if got := broken.recordedRulesRev(rev(0)); got != unreadableRulesRev {
		t.Errorf("unreadable rules: %d", got)
	}
	// Stored rules that do not verify keep their revision, and no claim
	// of an older one is taken; the hook's own "unreadable" is.
	damaged := &termsRules{cur: spaces.StoredRules{Rev: 4}, problem: "do not verify"}
	for _, claimed := range []*int64{rev(0), rev(3), nil} {
		if got := damaged.recordedRulesRev(claimed); got != 4 {
			t.Errorf("damaged rules, claimed %v: %d", claimed, got)
		}
	}
	if got := r.recordedRulesRev(rev(unreadableRulesRev)); got != unreadableRulesRev {
		t.Errorf("hook's unreadable: %d", got)
	}
	if got := (*termsRules)(nil).recordedRulesRev(rev(2)); got != 0 {
		t.Errorf("no store: %d", got)
	}
}
