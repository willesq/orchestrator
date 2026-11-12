package hook

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Outbound terms (spaces plan SP-044): before a call that sends text out
// (git commit, push and tag, gh pr, issue and release writes, gh api
// writes), pre-tool-use checks the text against every space's terms and
// refuses it when it would carry a term to a destination outside that
// term's space. While the terms layer is on (SP-070) the check fails
// closed: text it cannot read is refused. The refusal names the term's
// space, line and hash, never the term (INV-07).

// termsTimeout bounds the check, git included.
const termsTimeout = 5 * time.Second

// checkOutbound runs the check; tests replace it.
var checkOutbound = defaultCheckOutbound

// refuseOutboundTerms prints a deny and reports true when the call would
// send another space's term out.
func refuseOutboundTerms(p Payload, out io.Writer) (refused bool) {
	ctx, cancel := context.WithTimeout(context.Background(), termsTimeout)
	defer cancel()
	defer func() {
		// Reason: a check that panics cannot vouch for the call (INV-07).
		if recover() != nil {
			writeTermsDeny(out, spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundUnchecked,
				Kind: "a command that may send text out"}.Reason())
			refused = true
		}
	}()
	r := checkOutbound(ctx, p)
	if !r.Refuse {
		return false
	}
	writeTermsDeny(out, r.Reason())
	return true
}

// defaultCheckOutbound reads the terms layer's switch, the applied policy
// and the terms files, then checks the call.
func defaultCheckOutbound(ctx context.Context, p Payload) spaces.OutboundResult {
	acts, unknown := spaces.OutboundActions(p.ToolName, p.ToolInput, p.Cwd)
	if len(acts) == 0 && !unknown {
		return spaces.OutboundResult{}
	}
	db := paths.DBFile()
	if sw, err := spaces.ReadEnforcement(ctx, db); err == nil && sw.At(time.Now()).State(spaces.LayerTerms) == spaces.SwitchOff {
		return spaces.OutboundResult{}
	}
	home, _ := os.UserHomeDir()
	res, err := spaces.ReadApplied(ctx, db, spaces.Env{Home: home})
	if err != nil {
		// Reason: the terms cannot be known, so text cannot be vouched
		// for; only a call that sends something out is refused.
		kind := "a command that may send text out"
		if len(acts) > 0 {
			kind = acts[0].Kind
		}
		return spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundUnchecked, Kind: kind}
	}
	lists, errs := spaces.SpaceTerms(res, home)
	c := spaces.OutboundChecker{Policy: res, Terms: lists, TermErrs: errs, Home: home}
	return c.Check(ctx, p.ToolName, p.ToolInput, p.Cwd)
}

// writeTermsDeny prints the PreToolUse deny Claude Code and Codex honor
// (SP-001), with a reason that names no term.
func writeTermsDeny(out io.Writer, reason string) {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	_, _ = out.Write(append(b, '\n'))
}
