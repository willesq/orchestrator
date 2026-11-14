package spaces

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// recordAs stores d the way orchd's command log does: a decision the guard
// enforced as its decision and rule, one it did not as "would ..." in the
// rule (daemon wouldRule).
func recordAs(d GuardDecision) (decision, rule string) {
	if d.Enforced {
		if d.Verdict == Refuse {
			return "refused", string(d.Rule)
		}
		return "allowed", string(d.Rule)
	}
	return "logged", fmt.Sprintf("would %s (%s)", d.Would, d.Rule)
}

// replayGroup is the guard cases that share a pin and a policy: one
// session's log.
type replayGroup struct {
	pin    GuardPin
	pol    GuardPolicy
	cases  []guardCase
	events []ReplayEvent
}

// recordCases groups the guard cases into sessions and records each case's
// decision in mode, as the live guard would have logged it.
func recordCases(t *testing.T, f guardFixture, mode GuardMode) []*replayGroup {
	t.Helper()
	var groups []*replayGroup
	byKey := map[string]*replayGroup{}
	for i, c := range guardCases(t, f) {
		pol := f.pol
		if c.pol != nil {
			pol = *c.pol
		}
		key := fmt.Sprintf("%v|%s|%v", c.pin.Found, c.pin.Space, c.pol != nil)
		g := byKey[key]
		if g == nil {
			g = &replayGroup{pin: c.pin, pol: pol}
			byKey[key] = g
			groups = append(groups, g)
		}
		d := Decide(pol, c.pin, GuardEnforcement{Guard: mode}, c.ev)
		dec, rule := recordAs(d)
		g.cases = append(g.cases, c)
		g.events = append(g.events, ReplayEvent{Seq: int64(i + 1), PolicyRev: 1, Guard: c.ev, Recorded: RecordedVerdict(dec, rule)})
	}
	return groups
}

func fixedPolicies(pol GuardPolicy) ReplayPolicies {
	return func(int64) (GuardPolicy, error) { return pol, nil }
}

// TestReplayReproducesRecordedDecisions is SP-047.1.a1: every guard case,
// recorded in each mode as the command log stores it, replays under the
// recording policy to the recorded verdict and rule: 0 mismatches.
func TestReplayReproducesRecordedDecisions(t *testing.T) {
	f := newGuardFixture(t)
	for _, mode := range []GuardMode{GuardOff, GuardLog, GuardBlockOtherJobs, GuardStrict} {
		t.Run(string(mode), func(t *testing.T) {
			total, mismatches := 0, 0
			for _, g := range recordCases(t, f, mode) {
				rep := Replay(g.events, g.pin, GuardEnforcement{Guard: mode}, fixedPolicies(g.pol))
				if rep.Compared != len(g.events) || rep.Unrecorded != 0 || rep.Problems != 0 {
					t.Errorf("pin %+v: compared %d of %d, unrecorded %d, problems %d", g.pin, rep.Compared, len(g.events), rep.Unrecorded, rep.Problems)
				}
				total += rep.Compared
				mismatches += rep.Changed
				for _, r := range rep.Rows {
					if r.Changed {
						t.Errorf("seq %d: recorded %+v, replayed %s (%s)", r.Event.Seq, r.Event.Recorded, r.Verdict, r.Rule)
					}
				}
			}
			t.Logf("mode %s: %d recorded decisions replayed, %d mismatches", mode, total, mismatches)
			if total < 40 || mismatches != 0 {
				t.Fatalf("replayed %d decisions with %d mismatches, want >= 40 and 0", total, mismatches)
			}
		})
	}
}

// TestReplayChangedPolicyListsRules is SP-047.1.a2: replayed under a policy
// where globex is no longer another job, each call whose verdict changes is
// listed with its new rule id, and only those.
func TestReplayChangedPolicyListsRules(t *testing.T) {
	f := newGuardFixture(t)
	changed := placementPolicy
	changed.Spaces = append([]Space(nil), placementPolicy.Spaces...)
	changed.Spaces[3].Kind = Public
	env := testEnv()
	env.Home = f.home
	res, err := Validate(changed, env)
	if err != nil {
		t.Fatal(err)
	}
	newPol, err := NewGuardPolicy(res, []string{filepath.Join(f.home, ".claude"), filepath.Join(f.acmeApp, ".claude", "settings.local.json")})
	if err != nil {
		t.Fatal(err)
	}
	enf := GuardEnforcement{Guard: GuardBlockOtherJobs}
	listed := map[string]string{}
	for _, g := range recordCases(t, f, GuardBlockOtherJobs) {
		pol := newPol
		if g.pol.res == nil {
			pol = g.pol // the no-policy case keeps having none
		}
		rep := Replay(g.events, g.pin, enf, fixedPolicies(pol))
		for i, r := range rep.Rows {
			now := Decide(pol, g.pin, enf, g.events[i].Guard)
			before := g.events[i].Recorded
			wantChanged := now.Would != before.Verdict || now.Rule != before.Rule
			if r.Changed != wantChanged || r.Verdict != now.Would || r.Rule != now.Rule || r.Rule == "" {
				t.Errorf("%s: row %+v, want changed=%v to %s (%s)", g.cases[i].name, r, wantChanged, now.Would, now.Rule)
			}
			if r.Changed {
				listed[g.cases[i].name] = fmt.Sprintf("%s (%s) -> %s (%s)", before.Verdict, before.Rule, r.Verdict, r.Rule)
			}
		}
	}
	for name, want := range map[string]string{
		"cd into another job":                         "refuse (other_job_path) -> allow (within_space)",
		"Read tool into another job":                  "refuse (other_job_path) -> allow (within_space)",
		"gh api into another job's org":               "refuse (other_job_repo) -> allow (within_space)",
		"grep -r over a folder holding another job":   "refuse (other_job_ancestor) -> allow (within_space)",
		"/cd into another job is logged, not refused": "refuse (other_job_path) -> allow (within_space)",
	} {
		if listed[name] != want {
			t.Errorf("%s: listed %q, want %q", name, listed[name], want)
		}
	}
	if _, ok := listed["cd within the space"]; ok {
		t.Error("an unchanged verdict was listed")
	}
	t.Logf("%d changed verdicts listed: %v", len(listed), listed)
}

func TestReplayUnrecordedAndProblems(t *testing.T) {
	f := newGuardFixture(t)
	events := []ReplayEvent{
		{Seq: 1, PolicyRev: 1, Guard: GuardEventOf("pre-tool-use", "Bash", f.acmeApp, "", "", `{"paths":["`+f.globexApp+`"]}`)},
		{Seq: 2, PolicyRev: 2, Guard: GuardEventOf("pre-tool-use", "Bash", f.acmeApp, "", "", "")},
		{Seq: 3, PolicyRev: 2, Guard: GuardEventOf("pre-tool-use", "Bash", f.acmeApp, "", "", "")},
	}
	calls := 0
	rep := Replay(events, acmePin, GuardEnforcement{}, func(rev int64) (GuardPolicy, error) {
		calls++
		if rev == 2 {
			return GuardPolicy{}, errors.New("revision 2 was changed outside orchd")
		}
		return f.pol, nil
	})
	if rep.Unrecorded != 1 || rep.Problems != 2 || rep.Compared != 0 || calls != 2 {
		t.Fatalf("report = %+v, %d policy reads", rep, calls)
	}
	if r := rep.Rows[0]; r.Verdict != Refuse || r.Rule != RuleOtherJobPath || r.Changed {
		t.Errorf("unrecorded row = %+v", r)
	}
	if r := rep.Rows[1]; r.Problem == "" || r.Verdict != "" {
		t.Errorf("problem row = %+v", r)
	}
}

func TestRecordedVerdict(t *testing.T) {
	for _, tc := range []struct {
		decision, rule string
		want           RecordedDecision
	}{
		{"logged", "would refuse (other_job_path)", RecordedDecision{true, Refuse, RuleOtherJobPath}},
		{"not enforced", "would allow (within_space)", RecordedDecision{true, Allow, RuleWithinSpace}},
		{"logged", "would allow", RecordedDecision{true, Allow, ""}},
		{"logged", "would maybe (x)", RecordedDecision{}},
		{"refused", "other_job_repo", RecordedDecision{true, Refuse, RuleOtherJobRepo}},
		{"allowed", "within_space", RecordedDecision{true, Allow, RuleWithinSpace}},
		{"logged", "", RecordedDecision{}},
		{"not enforced", "", RecordedDecision{}},
	} {
		if got := RecordedVerdict(tc.decision, tc.rule); got != tc.want {
			t.Errorf("RecordedVerdict(%q, %q) = %+v, want %+v", tc.decision, tc.rule, got, tc.want)
		}
	}
}

func TestGuardEventOf(t *testing.T) {
	ev := GuardEventOf("cwd-changed", "", "/new", "/old", "/new", `{"paths":["/x"]}`)
	if ev.Kind != EventCwdChanged || ev.Cwd != "/old" || len(ev.Targets.CdDest) != 1 || ev.Targets.CdDest[0] != "/new" || len(ev.Targets.Paths) != 0 {
		t.Errorf("cwd-changed = %+v", ev)
	}
	if ev := GuardEventOf("cwd-changed", "", "/here", "", "", ""); ev.Cwd != "/here" || len(ev.Targets.CdDest) != 0 {
		t.Errorf("cwd-changed without cwds = %+v", ev)
	}
	ev = GuardEventOf("pre-tool-use", "Bash", "/w", "", "", `{"paths":["/a"],"gh_repos":["o/r"]}`)
	if ev.Tool != "Bash" || ev.Cwd != "/w" || ev.Targets.Paths[0] != "/a" || ev.Targets.GHRepos[0] != "o/r" {
		t.Errorf("pre-tool-use = %+v", ev)
	}
	if ev := GuardEventOf("pre-tool-use", "Bash", "/w", "", "", "{broken"); !ev.Targets.Opaque {
		t.Errorf("unreadable targets = %+v, want opaque", ev.Targets)
	}
}

// TestEngineRevisionsForReplay: a saved revision is read and validated
// without being applied; a missing or edited one is an error.
func TestEngineRevisionsForReplay(t *testing.T) {
	db := openStore(t)
	e := openEngine(t, db)
	if res, err := e.ResolvedRevision(context.Background(), 0); res != nil || err != nil {
		t.Fatalf("revision 0 = %v, %v", res, err)
	}
	r, _, err := e.SaveDraft(context.Background(), placementPolicy, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.ResolvedRevision(context.Background(), r.Number)
	if err != nil || !res.Configured() {
		t.Fatalf("revision %d = %v, %v", r.Number, res, err)
	}
	if cur, _ := e.Current(); cur != nil {
		t.Error("reading a revision applied it")
	}
	if _, err := e.ResolvedRevision(context.Background(), 99); !errors.Is(err, ErrNoRevision) {
		t.Errorf("missing revision: %v", err)
	}
	if _, err := db.Exec(`UPDATE spaces_revisions SET doc = '{"spaces":[]}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResolvedRevision(context.Background(), r.Number); !errors.Is(err, ErrTampered) {
		t.Errorf("edited revision: %v", err)
	}
	if res, err := e.Resolve(placementPolicy); err != nil || !res.Configured() {
		t.Errorf("Resolve = %v, %v", res, err)
	}
}

// TestReplayCarriesTermsVerdicts (RC-116): a recorded refusal by the
// outbound terms check is carried while the replayed guard lets the call
// through and the terms layer is on; a guard refusal replayed first, or the
// layer off, gives the guard's verdict. Other recorded verdicts are never
// carried.
func TestReplayCarriesTermsVerdicts(t *testing.T) {
	pol, err := NewGuardPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := func(rule GuardRule) ReplayEvent {
		return ReplayEvent{Guard: GuardEvent{Kind: EventPreToolUse, Tool: "Bash"}, Recorded: RecordedVerdict("refused", string(rule))}
	}
	events := []ReplayEvent{ev(RuleOutboundTerm), ev(RuleOutboundUnchecked), ev(RuleOtherJobPath),
		{Guard: GuardEvent{Kind: EventPreToolUse, Tool: "Bash"}, Recorded: RecordedVerdict("allowed", string(RuleWithinSpace))}}
	for _, tc := range []struct {
		terms   string
		changed int
		rules   []GuardRule
	}{
		{"", 2, []GuardRule{RuleOutboundTerm, RuleOutboundUnchecked, RuleNoPolicy, RuleNoPolicy}},
		{SwitchOn, 2, []GuardRule{RuleOutboundTerm, RuleOutboundUnchecked, RuleNoPolicy, RuleNoPolicy}},
		{SwitchOff, 4, []GuardRule{RuleNoPolicy, RuleNoPolicy, RuleNoPolicy, RuleNoPolicy}},
	} {
		rep := Replay(events, GuardPin{}, GuardEnforcement{Guard: GuardStrict, Terms: tc.terms}, fixedPolicies(pol))
		if rep.Changed != tc.changed {
			t.Errorf("terms %q: changed %d, want %d", tc.terms, rep.Changed, tc.changed)
		}
		for i, row := range rep.Rows {
			if row.Rule != tc.rules[i] {
				t.Errorf("terms %q row %d: %s %s", tc.terms, i, row.Verdict, row.Rule)
			}
		}
	}
}
