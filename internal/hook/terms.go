package hook

import (
	"context"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// Outbound terms (spaces plan SP-044): before a call that sends text out
// (git commit, push and tag, gh pr, issue and release writes, gh api
// writes), pre-tool-use checks the text against every space's terms and
// refuses it when it would carry a term to a destination outside that
// term's space. It runs after the guard (guardToolEvent) lets a call
// through, from the policy and switches in the same snapshot (RC-116).
// While the terms layer is on (SP-070) the check fails closed: text it
// cannot read is refused. The refusal names the term's space, line and
// hash, never the term (INV-07). The check is best-effort static reading
// of shell (D-60).

// termsTimeout bounds the check, git included.
const termsTimeout = 5 * time.Second

// checkOutbound runs the check; tests replace it.
var checkOutbound = defaultCheckOutbound

// outboundTerms checks p against the terms the guard's snapshot names.
func outboundTerms(p Payload, g spaces.GuardTerms) (r spaces.OutboundResult) {
	ctx, cancel := context.WithTimeout(context.Background(), termsTimeout)
	defer cancel()
	defer func() {
		// Reason: a check that panics cannot vouch for the call (INV-07).
		if recover() != nil {
			r = spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundUnchecked,
				Kind: "a command that may send text out"}
		}
	}()
	return checkOutbound(ctx, p, g)
}

// defaultCheckOutbound checks the call against g: the terms layer's switch
// and the applied policy, whose terms files it reads.
func defaultCheckOutbound(ctx context.Context, p Payload, g spaces.GuardTerms) spaces.OutboundResult {
	if g.State == spaces.SwitchOff {
		return spaces.OutboundResult{}
	}
	acts, unknown := spaces.OutboundActions(p.ToolName, p.ToolInput, p.Cwd)
	if len(acts) == 0 && !unknown {
		return spaces.OutboundResult{}
	}
	if !g.Known {
		// Reason: the terms cannot be known, so text cannot be vouched
		// for; only a call that sends something out is refused.
		kind := "a command that may send text out"
		if len(acts) > 0 {
			kind = acts[0].Kind
		}
		return spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundUnchecked, Kind: kind}
	}
	home, _ := os.UserHomeDir()
	termsHome := g.Home
	if termsHome == "" {
		termsHome = home
	}
	lists, errs := spaces.SpaceTerms(g.Policy, termsHome)
	c := spaces.OutboundChecker{Policy: g.Policy, Terms: lists, TermErrs: errs, Home: home}
	return c.Check(ctx, p.ToolName, p.ToolInput, p.Cwd)
}
