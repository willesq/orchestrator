package spaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestOutboundRulesStore is the store half of SP-075.1.a3: the baseline
// until changed, each change stored as a new revision with the whole rule
// set and read back, a history without values, and rows changed outside
// orchd found.
func TestOutboundRulesStore(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	s, problem, err := LoadOutboundRules(ctx, conn)
	if err != nil || problem != "" || s.Rev != 0 || len(s.Rules.User) != 0 {
		t.Fatalf("empty store: %+v %q %v", s, problem, err)
	}
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	var cur OutboundRules
	steps := []struct {
		action          RuleAction
		id, kind, value string
	}{
		{RuleAdd, "", "fail_closed", "deploy-prod"},
		{RuleDisable, string(OutboundPendingWork), "", ""},
		{RuleRemove, "user-1", "", ""},
	}
	for i, st := range steps {
		next, c, changed, err := ChangeOutboundRules(cur, st.action, st.id, st.kind, st.value)
		if err != nil || !changed {
			t.Fatalf("step %d: %v %v", i, changed, err)
		}
		c.By, c.At = "operator (process 1)", t0.Add(time.Duration(i+1)*time.Minute)
		c, err = WriteOutboundRules(ctx, conn, c, next)
		if err != nil || c.Rev != int64(i+1) {
			t.Fatalf("write %d: %+v %v", i, c, err)
		}
		cur = next
	}
	s, problem, err = LoadOutboundRules(ctx, conn)
	if err != nil || problem != "" || s.Rev != 3 || s.By != "operator (process 1)" || !s.At.Equal(t0.Add(3*time.Minute)) ||
		s.Rules.On(OutboundPendingWork) || len(s.Rules.User) != 0 || s.Rules.LastID != 1 {
		t.Fatalf("load: %+v %q %v", s, problem, err)
	}
	hist, err := OutboundRulesHistory(ctx, conn, 2)
	if err != nil || len(hist) != 2 || hist[0].Action != RuleRemove || hist[1].RuleID != string(OutboundPendingWork) {
		t.Fatalf("history: %+v %v", hist, err)
	}
	for _, h := range hist {
		if strings.Contains(h.Describe(), "deploy") {
			t.Errorf("description %q holds a value", h.Describe())
		}
	}

	// The timeline gives each revision's rules (D-66).
	tl, err := LoadRulesTimeline(ctx, conn)
	if err != nil || len(tl) != 3 {
		t.Fatalf("timeline: %v %v", tl, err)
	}
	for rev, want := range map[int64]int{0: -1, 1: 1, 2: 1, 3: 0} {
		r, err := tl.Rev(rev)
		switch {
		case err != nil:
			t.Errorf("rev %d: %v", rev, err)
		case want < 0 && r != nil:
			t.Errorf("rev %d: %+v, want the baseline", rev, r)
		case want >= 0 && (r == nil || len(r.User) != want):
			t.Errorf("rev %d: %+v, want %d added", rev, r, want)
		}
	}
	if _, err := tl.Rev(9); !errors.Is(err, ErrRulesUnreadable) {
		t.Errorf("a revision the store does not hold: %v", err)
	}

	// A change made outside orchd does not verify.
	if _, err := conn.Exec(`UPDATE spaces_terms_rules SET rules = '{}' WHERE rev = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE spaces_terms_rules SET at = 'yesterday', hash = 'x' WHERE rev = 3`); err != nil {
		t.Fatal(err)
	}
	if s, problem, err := LoadOutboundRules(ctx, conn); err != nil || problem == "" || s.Rev != 3 || strings.Contains(problem, "deploy") {
		t.Errorf("tampered: %+v %q %v", s, problem, err)
	}
	tl, err = LoadRulesTimeline(ctx, conn)
	if err != nil || tl[0].Broken || !tl[1].Broken || !tl[2].Broken {
		t.Fatalf("timeline of tampered rows: %+v %v", tl, err)
	}
	if _, err := tl.Rev(2); !errors.Is(err, ErrRulesUnreadable) {
		t.Errorf("event under a broken row: %v", err)
	}

	// Invalid rules and unknown changes are not stored.
	if _, err := WriteOutboundRules(ctx, conn, RulesChange{Action: RuleAdd}, OutboundRules{Off: []OutboundRule{"x"}}); err == nil {
		t.Error("invalid rules stored")
	}
	if _, _, _, err := ChangeOutboundRules(cur, "rename", "user-1", "", ""); !errors.Is(err, ErrBadRule) {
		t.Errorf("unknown change: %v", err)
	}
	// A closed store is an error, not the baseline.
	conn.Close()
	if _, _, err := LoadOutboundRules(ctx, conn); err == nil {
		t.Error("closed store loaded")
	}
	if _, err := LoadRulesTimeline(ctx, conn); err == nil {
		t.Error("closed store timeline loaded")
	}
	if _, err := OutboundRulesHistory(ctx, conn, 1); err == nil {
		t.Error("closed store history loaded")
	}
	if _, err := WriteOutboundRules(ctx, conn, RulesChange{}, OutboundRules{}); err == nil {
		t.Error("closed store written")
	}
}

// TestReplayWithRules is SP-075.1.a3 in the replay: an event is judged
// with the rules revision it was recorded with, only while the policy names
// a terms file, and a recorded terms verdict is still carried.
func TestReplayWithRules(t *testing.T) {
	res := mustValid(t, Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Roots: []string{"/w/acme"}, TermsFile: "/t/acme.txt"},
	}}, testEnv())
	pol, err := NewGuardPolicy(res, nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := func(int64) (GuardPolicy, error) { return pol, nil }
	strict, _, _ := OutboundRules{}.Add("fail_closed", "deploy-prod")
	rules := func(rev int64) (*OutboundRules, error) {
		switch rev {
		case 0:
			return nil, nil
		case 1:
			return &strict, nil
		case 2:
			return &OutboundRules{Off: []OutboundRule{"nope"}}, nil
		}
		return nil, ErrRulesUnreadable
	}
	// Events are numbered by the rules revision they were recorded with.
	ev := func(rev int64, cmd string, rec RecordedDecision) ReplayEvent {
		return ReplayEvent{RulesRev: rev, Command: cmd, Recorded: rec,
			Guard: GuardEvent{Kind: EventPreToolUse, Tool: "Bash", Cwd: "/w/acme"}}
	}
	allowed := RecordedDecision{OK: true, Verdict: Allow, Rule: RuleWithinSpace}
	termRefused := RecordedDecision{OK: true, Verdict: Refuse, Rule: RuleOutboundTerm}
	events := []ReplayEvent{
		ev(0, "deploy-prod now", allowed),
		ev(1, "deploy-prod now", allowed),
		ev(1, "ls", allowed),
		ev(1, "deploy-prod now", termRefused),
		ev(2, "deploy-prod now", allowed),
		ev(3, "deploy-prod now", allowed),
		ev(1, "", allowed),
	}
	pin := GuardPin{Found: true, Space: "acme"}
	rep := ReplayWithRules(events, pin, GuardEnforcement{Guard: GuardLog}, policies, rules)
	want := []struct {
		rule    GuardRule
		problem bool
	}{
		{RuleWithinSpace, false}, {RuleOutboundUnchecked, false}, {RuleWithinSpace, false}, {RuleOutboundTerm, false},
		{"", true}, {"", true}, {RuleWithinSpace, false},
	}
	for i, w := range want {
		row := rep.Rows[i]
		if row.Rule != w.rule || (row.Problem != "") != w.problem {
			t.Errorf("row %d: %+v, want %s problem %v", i, row, w.rule, w.problem)
		}
	}
	if rep.Changed != 1 || rep.Problems != 2 {
		t.Errorf("report: changed %d problems %d", rep.Changed, rep.Problems)
	}
	// With the terms layer off, or no terms file in the policy, nothing is
	// judged; Replay judges no rules.
	for name, r := range map[string]ReplayReport{
		"terms off": ReplayWithRules(events[1:2], pin, GuardEnforcement{Guard: GuardLog, Terms: SwitchOff}, policies, rules),
		"no rules":  Replay(events[1:2], pin, GuardEnforcement{Guard: GuardLog}, policies),
		"no terms": ReplayWithRules(events[1:2], pin, GuardEnforcement{Guard: GuardLog}, func(int64) (GuardPolicy, error) {
			p, _ := NewGuardPolicy(mustValid(t, Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, Roots: []string{"/w/acme"}}}}, testEnv()), nil)
			return p, nil
		}, rules),
	} {
		if r.Rows[0].Rule != RuleWithinSpace {
			t.Errorf("%s: %+v", name, r.Rows[0])
		}
	}
}
