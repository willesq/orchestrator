package daemon

import (
	"regexp"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// decisionAllowed is the decision of a call the enforcing guard allowed.
const decisionAllowed = "allowed"

// guardRulePattern is what a rule id the hook reports may look like. Rule
// ids name a rule, never a path or text; anything else is stored as
// unknown.
var guardRulePattern = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)

// guardRefused reports whether the hook's guard refused the call: such
// events are the guard decisions the command log keeps room for (D-37).
// Reason: only an enforced refusal of a pinned session or a task gets the
// room; otherwise any process could fill it with claimed refusals from
// processes it names claude.
func guardRefused(req *orchdv1.RecordToolEventRequest, owner toollog.Owner) bool {
	return req.GetGuardVerdict() == "refuse" && req.GetGuardEnforced() && owner.Kind != toollog.OwnerProcess
}

// applyHookDecision records the guard's decision the hook reported (SP-041)
// on r. An enforced verdict becomes the decision, refused or allowed, with
// its rule. One not enforced keeps decision logged, with the verdict the
// guard would have given in the rule (RC-86); markGuardState then marks the
// guard's switch. The decision is the hook's report of what it did: any
// caller can only write to the chain orchd resolved for it.
func applyHookDecision(r *toollog.Record, req *orchdv1.RecordToolEventRequest) {
	v := req.GetGuardVerdict()
	if v != "allow" && v != "refuse" {
		return
	}
	rule := req.GetGuardRule()
	if !guardRulePattern.MatchString(rule) {
		rule = "unknown"
	}
	if !req.GetGuardEnforced() {
		r.Rule = wouldRule(v, rule)
		return
	}
	r.Decision, r.Rule = decisionAllowed, rule
	if v == "refuse" {
		r.Decision = toollog.DecisionRefused
	}
}
