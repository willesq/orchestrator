package daemon

import (
	"context"
	"log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The outbound rules RPCs (spaces SP-075). See docs/internals/daemon.md
// "Outbound rules".

// rulesHistoryLimit is how many changes ListOutboundRules returns.
const rulesHistoryLimit = 50

// ListOutboundRules lists the rules without any value (INV-07). Open to
// every caller; who changed them, and the history, only to the operator.
func (s *server) ListOutboundRules(ctx context.Context, _ *orchdv1.ListOutboundRulesRequest) (*orchdv1.OutboundRules, error) {
	return s.outboundRulesProto(ctx), nil
}

// ChangeOutboundRule makes one change, as the operator only (INV-06).
func (s *server) ChangeOutboundRule(ctx context.Context, req *orchdv1.ChangeOutboundRuleRequest) (*orchdv1.ChangeOutboundRuleResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ChangeOutboundRule_FullMethodName, "change the outbound rules"); err != nil {
		return nil, err
	}
	action := spaces.RuleAction(req.GetAction())
	switch action {
	case spaces.RuleEnable, spaces.RuleDisable, spaces.RuleRemove:
		if req.GetId() == "" {
			return nil, grpcstatus.Error(codes.InvalidArgument, "name the rule's id")
		}
	case spaces.RuleAdd:
	default:
		return nil, grpcstatus.Error(codes.InvalidArgument, "action must be enable, disable, add or remove")
	}
	c, changed, err := s.changeOutboundRule(ctx, orchdv1.Orchd_ChangeOutboundRule_FullMethodName, action,
		req.GetId(), req.GetKind(), req.GetValue())
	if err != nil {
		return nil, err
	}
	cur, _ := s.termsRules.current()
	return &orchdv1.ChangeOutboundRuleResponse{RuleId: c.RuleID, Changed: changed, Revision: cur.Rev,
		Current: s.outboundRulesProto(ctx)}, nil
}

// outboundRulesProto is the rules for the wire: ids, kinds and states.
func (s *server) outboundRulesProto(ctx context.Context) *orchdv1.OutboundRules {
	cur, problem := s.termsRules.current()
	rules := cur.Rules
	if problem != "" {
		// Reason: rules that cannot be read are not applied; listing them
		// as they were would show a state the check does not have.
		rules = spaces.OutboundRules{}
	}
	out := &orchdv1.OutboundRules{Revision: cur.Rev, SetAt: unixOrZero(cur.At), Problem: problem,
		TermsLayer: s.enforcement.state(spaces.LayerTerms)}
	for _, k := range spaces.UserRuleKinds {
		out.Kinds = append(out.Kinds, string(k))
	}
	for _, v := range outboundRuleViews(rules) {
		out.Rules = append(out.Rules, &orchdv1.OutboundRule{Id: v.ID, Kind: v.Kind, On: v.On, Loosens: v.Loosens,
			Summary: v.Summary, Baseline: v.Baseline})
	}
	if _, ok := operator(ctx); !ok || s.termsRules == nil {
		return out
	}
	out.SetBy = cur.By
	hist, err := spaces.OutboundRulesHistory(ctx, s.termsRules.db, rulesHistoryLimit)
	if err != nil {
		log.Printf("orchd: outbound rules: %v", err)
	}
	for _, h := range hist {
		out.History = append(out.History, &orchdv1.OutboundRuleChange{Revision: h.Rev, At: unixOrZero(h.At), By: h.By,
			Action: string(h.Action), RuleId: h.RuleID})
	}
	return out
}

// recordedRulesRev is the rules revision to record with a tool event
// (D-66): the one the hook says it judged under, when it is one orchd has
// applied (0 up to orchd's current one) or the hook's "rules unreadable"
// (-1), else orchd's own. Reason: the hook's is a claim, kept only when it
// could be true; while orchd's own rules cannot be read none is taken, so
// a replay never judges such a call under older, looser rules.
func (t *termsRules) recordedRulesRev(claimed *int64) int64 {
	own := t.revision()
	if claimed == nil {
		return own
	}
	if *claimed == unreadableRulesRev {
		return unreadableRulesRev
	}
	if _, problem := t.current(); problem == "" && *claimed >= 0 && *claimed <= own {
		return *claimed
	}
	return own
}
