package spaces

import (
	"context"
	"encoding/json"
	"strings"
)

// Policy replay (SP-047) decides recorded tool events again, with Decide, and
// reports where the verdict differs from the one recorded. It only reads:
// the events, a policy and a pin go in, a report comes out.

// ReplayEvent is one recorded event to decide again.
type ReplayEvent struct {
	Seq int64
	// At is when it was recorded, unix nanoseconds; 0 when unknown.
	At int64
	// Guard is the event as Decide takes it (GuardEventOf).
	Guard GuardEvent
	// PolicyRev is the spaces revision applied when it was recorded; 0 for
	// none.
	PolicyRev int64
	// Recorded is the verdict recorded with it; ok is false when the record
	// carries none (RecordedVerdict).
	Recorded RecordedDecision
}

// RecordedDecision is a verdict and rule as a log recorded them.
type RecordedDecision struct {
	OK      bool
	Verdict Verdict
	Rule    GuardRule
}

// ReplayRow is one event's outcome. Its event keeps only the guard event's
// kind and tool.
type ReplayRow struct {
	Event ReplayEvent
	// Verdict and Rule are the replay's: the verdict of the mode replayed
	// in, or of the default mode when that mode does not enforce (Decide's
	// Would), so they compare with what a logging guard records; for a
	// call the terms check refused, its recorded verdict (Replay).
	Verdict Verdict
	Rule    GuardRule
	// Changed is set when the event has a recorded verdict and the replay's
	// verdict or rule differs from it.
	Changed bool
	// Problem says why the event could not be decided: its policy could
	// not be read. Verdict and Rule are then empty.
	Problem string
}

// ReplayReport is a replay's outcome, rows in the events' order.
type ReplayReport struct {
	Rows []ReplayRow
	// Compared counts events with a recorded verdict that were decided;
	// Changed those whose verdict or rule differs; Unrecorded events with
	// no recorded verdict; Problems events that could not be decided.
	Compared, Changed, Unrecorded, Problems int
}

// ReplayPolicies returns the guard policy to decide an event under, by the
// revision recorded with it. Replaying under one fixed policy ignores rev.
type ReplayPolicies func(rev int64) (GuardPolicy, error)

// Replay decides every event again under pin, enf and the policy policies
// gives for it. It calls the live guard's Decide, so a replay judges exactly
// as the guard does, and changes nothing. A call the outbound terms check
// refused keeps that verdict while the guard, replayed, lets it through and
// enf's terms layer is on, as the hook decides it (RC-116).
func Replay(events []ReplayEvent, pin GuardPin, enf GuardEnforcement, policies ReplayPolicies) ReplayReport {
	var rep ReplayReport
	type cached struct {
		pol GuardPolicy
		err error
	}
	seen := map[int64]cached{}
	for _, ev := range events {
		row := ReplayRow{Event: ev}
		c, ok := seen[ev.PolicyRev]
		if !ok {
			c.pol, c.err = policies(ev.PolicyRev)
			seen[ev.PolicyRev] = c
		}
		switch {
		case c.err != nil:
			row.Problem = c.err.Error()
			rep.Problems++
		default:
			d := Decide(c.pol, pin, enf, ev.Guard)
			row.Verdict, row.Rule = d.Would, d.Rule
			if d.Verdict != Refuse && enf.Terms != SwitchOff && carriesTerms(ev.Recorded) {
				// Reason: the hook checks terms only after the guard let
				// the call through, against text the log never keeps
				// (INV-07) and a checkout that has moved on since; the
				// recorded terms verdict is the one the call got.
				row.Verdict, row.Rule = Refuse, ev.Recorded.Rule
			}
			if ev.Recorded.OK {
				rep.Compared++
				row.Changed = row.Verdict != ev.Recorded.Verdict || row.Rule != ev.Recorded.Rule
				if row.Changed {
					rep.Changed++
				}
			} else {
				rep.Unrecorded++
			}
		}
		// Reason: a row outlives the decision in a report a caller may
		// hold for thousands of events, and only the kind and tool are
		// shown, so the targets and directories are dropped.
		row.Event.Guard = GuardEvent{Kind: ev.Guard.Kind, Tool: ev.Guard.Tool}
		rep.Rows = append(rep.Rows, row)
	}
	return rep
}

// carriesTerms reports whether r is a refusal by the outbound terms check
// (SP-044), which a replay carries rather than decides (RC-116).
func carriesTerms(r RecordedDecision) bool {
	return r.OK && r.Verdict == Refuse && (r.Rule == RuleOutboundTerm || r.Rule == RuleOutboundUnchecked)
}

// Recorded decision forms. orchd stores a call the guard decided as its
// decision ("refused", or "allowed") with the rule; a call it did not
// enforce keeps decision "logged" or "not enforced" and records the verdict
// it would have given in the rule: "would <verdict> (<rule>)".
const (
	recordedRefused = "refused"
	recordedAllowed = "allowed"
	recordedWould   = "would "
)

// RecordedVerdict reads the verdict and rule a command log record carries
// in its decision and rule fields. ok is false when it carries none: an
// event logged while no guard decided.
func RecordedVerdict(decision, rule string) RecordedDecision {
	if rest, ok := strings.CutPrefix(rule, recordedWould); ok {
		v, r, _ := strings.Cut(rest, " (")
		r = strings.TrimSuffix(r, ")")
		switch Verdict(v) {
		case Allow, Refuse:
			return RecordedDecision{OK: true, Verdict: Verdict(v), Rule: GuardRule(r)}
		}
		return RecordedDecision{}
	}
	switch decision {
	case recordedRefused:
		return RecordedDecision{OK: true, Verdict: Refuse, Rule: GuardRule(rule)}
	case recordedAllowed:
		return RecordedDecision{OK: true, Verdict: Allow, Rule: GuardRule(rule)}
	}
	return RecordedDecision{}
}

// recordCwdChanged is the cwd-changed event as the command log stores it.
const recordCwdChanged = "cwd-changed"

// GuardEventOf builds the event Decide takes from a command log record's
// fields: its event kind, tool, cwds and stored targets JSON. A cwd-changed
// event runs in its old directory and moves to its new one. Targets that do
// not parse are opaque, so an enforcing replay refuses them as the guard
// would refuse a call it cannot read.
func GuardEventOf(event, tool, cwd, oldCwd, newCwd, targets string) GuardEvent {
	ev := GuardEvent{Kind: GuardEventKind(event), Tool: tool, Cwd: cwd}
	if event == recordCwdChanged {
		if oldCwd != "" {
			ev.Cwd = oldCwd
		}
		if newCwd != "" {
			ev.Targets.CdDest = []string{newCwd}
		}
		return ev
	}
	if targets != "" {
		if err := json.Unmarshal([]byte(targets), &ev.Targets); err != nil {
			ev.Targets = ToolTargets{Opaque: true, OpaqueReasons: []string{"stored targets cannot be read"}}
		}
	}
	return ev
}

// ResolvedRevision reads stored revision n, checks its hash and validates
// it, for replaying events recorded under it. It writes nothing. A revision
// that no longer validates (a terms file moved into a git tree, say) is an
// error, not a policy.
func (e *Engine) ResolvedRevision(ctx context.Context, n int64) (*Resolved, error) {
	if n <= 0 {
		// No revision was applied: a nil policy, which NewGuardPolicy
		// takes as none configured, so every call is allowed (no_policy).
		return nil, nil
	}
	r, err := Get(ctx, e.db, n)
	if err != nil {
		return nil, err
	}
	return verify(r, "", e.env)
}

// Resolve validates p as the engine validates a draft, for replaying under
// a policy that was never saved. It writes nothing.
func (e *Engine) Resolve(p Policy) (*Resolved, error) {
	return Validate(p, e.env)
}
