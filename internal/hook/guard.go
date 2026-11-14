package hook

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The enforcing tool guard (spaces plan SP-041). pre-tool-use decides each
// call itself, from the files orchd writes (spaces.GuardFiles), and prints a
// deny when the guard refuses. This is the one exception to the hook's
// never-block contract: while the guard enforces, a decision that times out
// or cannot read its inputs refuses the call (INV-13). With the guard off or
// logging, or no spaces configured, the hook still prints nothing.

// guardTimeout bounds one decision, file reads included. Targets stops at
// its own 100 ms limit (and the call is then refused as analysis_limit), so
// only a stalled file read or a stopped process reaches this.
var guardTimeout = 250 * time.Millisecond

// guardFilesFor returns the guard's inputs; tests replace it.
var guardFilesFor = func() spaces.GuardFiles {
	return spaces.GuardFiles{Dir: paths.GuardDir(), DB: paths.DBFile()}
}

// decideGuard runs one decision; tests replace it to stall a decision.
// onMode is called once the guard's mode is known.
var decideGuard = func(ctx context.Context, f spaces.GuardFiles, in spaces.GuardInput, onMode func(spaces.GuardMode)) spaces.GuardResult {
	f.OnMode = onMode
	return f.Decide(ctx, in)
}

// guardToolEvent decides a pre-tool-use or cwd-changed event, prints the
// deny when the call is refused, and hands the event with its decision to
// orchd for the command log. A call the guard lets through is then checked
// for outbound terms (SP-044), from the same snapshot; its refusal is
// printed and recorded as the guard's are (RC-116).
func guardToolEvent(event string, p Payload, out io.Writer, cf clientFactory) {
	res := decideWithin(guardTimeout, guardInput(event, p))
	reason := ""
	if event == "pre-tool-use" && res.Verdict != spaces.Refuse {
		if t := outboundTerms(p, res.Terms); t.Refuse {
			res = termsRefusal(res, t)
			reason = t.Reason()
		}
	}
	if res.Verdict == spaces.Refuse && event == "pre-tool-use" {
		writeDeny(out, res.Rule, reason)
	}
	recordToolEvent(event, p, cf, &res)
}

// termsRefusal is g refused by the terms check's result t. The terms layer
// has its own switch, so the refusal takes effect whatever the guard's
// mode; the command log records it as refused with t's rule.
func termsRefusal(g spaces.GuardResult, t spaces.OutboundResult) spaces.GuardResult {
	g.Verdict, g.Would, g.Rule, g.Enforced = spaces.Refuse, spaces.Refuse, t.Rule, true
	return g
}

// guardInput is the decision's input for event. A cwd-changed event runs
// in its old directory and moves to its new one, as a replay of the
// command log builds it (spaces.GuardEventOf, SP-047), so a live verdict
// and its replay agree.
func guardInput(event string, p Payload) spaces.GuardInput {
	in := spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: p.SessionID, Tool: p.ToolName,
		Input: p.ToolInput, Cwd: p.Cwd, NewCwd: p.NewCwd}
	if event == "cwd-changed" {
		in.Kind = spaces.EventCwdChanged
		if p.OldCwd != "" {
			in.Cwd = p.OldCwd
		}
	}
	return in
}

// decideWithin decides in, refusing when the decision takes longer than
// limit and the guard enforces, or its mode is not known by then.
func decideWithin(limit time.Duration, in spaces.GuardInput) spaces.GuardResult {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var mode atomic.Value
	done := make(chan spaces.GuardResult, 1)
	decide, filesFor := decideGuard, guardFilesFor
	go func() {
		// Reason: a panic here would end the hook with a status the
		// harness reads as its own error; a refusal is the fail-closed
		// answer, which TimedOut gives for an enforcing or unknown mode.
		defer func() {
			if r := recover(); r != nil {
				m, _ := mode.Load().(spaces.GuardMode)
				done <- spaces.TimedOut(m)
			}
		}()
		done <- decide(ctx, filesFor(), in, func(m spaces.GuardMode) { mode.Store(m) })
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		m, _ := mode.Load().(spaces.GuardMode)
		r := spaces.TimedOut(m)
		if in.Kind == spaces.EventCwdChanged {
			r.Verdict, r.Enforced = spaces.Allow, false
		}
		return r
	}
}

// guardReasons say why a call was refused, by rule. They name the rule and
// what to do, never the path, repo or text that matched.
var guardReasons = map[spaces.GuardRule]string{
	spaces.RulePinMissing:       "this session has no space pin (it started before orchd ran or orchd could not pin it); start a new session",
	spaces.RuleSelfProtection:   "it writes to a file orch protects",
	spaces.RuleOtherJobPath:     "it reaches a folder of another job's space",
	spaces.RuleOtherJobAncestor: "it reaches a folder that holds another job's space",
	spaces.RuleOtherJobRepo:     "it reaches a GitHub repo of another job's space",
	spaces.RuleUnreadableSpace:  "it reaches a space this session may not read (strict mode)",
	spaces.RuleMalformedTarget:  "a target is not an absolute path or owner/repo",
	spaces.RuleAnalysisLimit:    "the call is too large or complex to check",
	spaces.RuleOpaqueStrict:     "part of the call cannot be checked (strict mode)",
	spaces.RulePolicyUnreadable: "the spaces policy orchd wrote for the guard cannot be read; check that orchd is running",
	spaces.RuleGuardTimeout:     "the check did not finish in time",
}

// writeDeny prints the deny both Claude Code and Codex read from a
// PreToolUse hook (recorded in SP-001): hookSpecificOutput with
// permissionDecision "deny" and a non-empty reason, which Codex requires.
// reason, when set, says why instead of the rule's own text: the terms
// check's, which names the term's space, line and hash, never the term.
func writeDeny(out io.Writer, rule spaces.GuardRule, reason string) {
	if reason == "" {
		why, ok := guardReasons[rule]
		if !ok {
			why = "the guard refused it"
		}
		reason = "orch spaces guard refused this call (rule " + string(rule) + "): " + why + "."
	}
	reason += " The operator can see it with 'orch spaces log'."
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	_, _ = out.Write(append(b, '\n'))
}
