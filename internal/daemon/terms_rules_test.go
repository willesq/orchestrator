package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// rulesRig is s's outbound rules on orch.db with a fake clock, and the
// changes they emitted.
type rulesRig struct {
	t       *termsRules
	clock   *fakeClock
	emitted *[]spaces.RulesChange
}

// withTermsRules gives s outbound rules on its orch.db, as Run does; call it
// before withGuardFiles.
func withTermsRules(t *testing.T, s *server) rulesRig {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	clock := &fakeClock{t: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
	emitted := &[]spaces.RulesChange{}
	r := newTermsRules(context.Background(), conn)
	r.now = clock.now
	r.emit = func(c spaces.RulesChange) { *emitted = append(*emitted, c) }
	s.termsRules = r
	return rulesRig{t: r, clock: clock, emitted: emitted}
}

// captureLog sends orchd's log to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// ruleValues are the rule values the tests add; none may leave orch.db and
// the snapshot (INV-07).
var ruleValues = []string{"deploy-prod", "glab", "acme/public-sdk"}

func assertNoRuleValues(t *testing.T, where, text string) {
	t.Helper()
	for _, v := range ruleValues {
		if strings.Contains(strings.ToLower(text), v) {
			t.Errorf("%s holds rule value %q (INV-07): %s", where, v, text)
		}
	}
}

// TestOutboundRulesOperatorOnly is SP-075.2.a1 in orchd: enabling,
// disabling, adding and removing a rule is refused to callers descended
// from a harness or orchd (INV-06), and each change is logged with its time
// and caller, never a rule's value.
func TestOutboundRulesOperatorOnly(t *testing.T) {
	s := spacesServer(t)
	rig := withTermsRules(t, s)
	logs := captureLog(t)
	const method = "/orchd.v1.Orchd/ChangeOutboundRule"
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	changes := []struct {
		action          spaces.RuleAction
		id, kind, value string
	}{
		{spaces.RuleDisable, string(spaces.OutboundPlainVariables), "", ""},
		{spaces.RuleAdd, "", "fail_closed", "deploy-prod"},
		{spaces.RuleAdd, "", "command", "glab"},
		{spaces.RuleDisable, "user-2", "", ""},
		{spaces.RuleEnable, string(spaces.OutboundPlainVariables), "", ""},
		{spaces.RuleRemove, "user-2", "", ""},
	}
	for name, ctx := range map[string]context.Context{
		"claude session": callerCtx(callerAuth{pid: 7, harness: "claude"}),
		"codex session":  callerCtx(callerAuth{pid: 7, harness: "codex"}),
		"orchd's child":  callerCtx(callerAuth{pid: 7, spawned: true}),
		"queued task":    queued,
		"unreadable":     callerCtx(callerAuth{pid: 7, harnessErr: errors.New("gone")}),
		"no peer":        context.Background(),
	} {
		for _, c := range changes {
			if _, _, err := s.changeOutboundRule(ctx, method, c.action, c.id, c.kind, c.value); grpcstatus.Code(err) != codes.PermissionDenied {
				t.Errorf("%s: %s = %v, want PermissionDenied", name, c.action, err)
			}
		}
	}
	if cur, _ := rig.t.current(); cur.Rev != 0 || len(*rig.emitted) != 0 {
		t.Fatalf("a refused change took effect: %+v %v", cur, *rig.emitted)
	}
	wantIDs := []string{"plain_variables", "user-1", "user-2", "user-2", "plain_variables", "user-2"}
	for i, c := range changes {
		rig.clock.advance(time.Minute)
		got, changed, err := s.changeOutboundRule(operatorCtx, method, c.action, c.id, c.kind, c.value)
		if err != nil || !changed || got.RuleID != wantIDs[i] || got.By != "operator (process 42)" ||
			!got.At.Equal(rig.clock.now()) || got.Rev != int64(i+1) {
			t.Fatalf("change %d: %+v %v %v", i, got, changed, err)
		}
	}
	// Repeating a change is not one; a bad rule or id is refused and
	// stores nothing.
	if _, changed, err := s.changeOutboundRule(operatorCtx, method, spaces.RuleEnable, "user-1", "", ""); err != nil || changed {
		t.Errorf("enable an on rule: %v %v", changed, err)
	}
	for _, c := range []struct {
		action          spaces.RuleAction
		id, kind, value string
		code            codes.Code
	}{
		{spaces.RuleAdd, "", "allow", "glab", codes.InvalidArgument},
		{spaces.RuleAdd, "", "command", "git", codes.InvalidArgument},
		{spaces.RuleRemove, "fail_closed_unreadable", "", "", codes.InvalidArgument},
		{spaces.RuleDisable, "user-99", "", "", codes.NotFound},
		{"rename", "user-1", "", "", codes.InvalidArgument},
	} {
		_, _, err := s.changeOutboundRule(operatorCtx, method, c.action, c.id, c.kind, c.value)
		if grpcstatus.Code(err) != c.code {
			t.Errorf("%s %s: %v, want %s", c.action, c.id, err, c.code)
		}
		assertNoRuleValues(t, "error", err.Error())
	}
	if len(*rig.emitted) != len(changes) {
		t.Fatalf("emitted %d changes", len(*rig.emitted))
	}
	hist, err := spaces.OutboundRulesHistory(context.Background(), rig.t.db, 50)
	if err != nil || len(hist) != len(changes) {
		t.Fatalf("history: %v %v", hist, err)
	}
	for i, h := range hist {
		c := changes[len(changes)-1-i]
		if h.Action != c.action || h.By != "operator (process 42)" || h.At.IsZero() {
			t.Errorf("history %d: %+v", i, h)
		}
		assertNoRuleValues(t, "change description", h.Describe())
	}
	assertNoRuleValues(t, "orchd's log", logs.String())
	if !strings.Contains(logs.String(), "outbound rule user-1: add by operator (process 42)") {
		t.Errorf("change not logged: %s", logs.String())
	}
	views := outboundRuleViews(rig.t.cur.Rules)
	if len(views) != len(spaces.OutboundBaseline)+1 || views[len(views)-1].ID != "user-1" || !views[len(views)-1].On {
		t.Errorf("views: %+v", views)
	}
	for _, v := range views {
		assertNoRuleValues(t, "rule view", v.ID+v.Kind+v.Summary)
		if v.Summary == "" {
			t.Errorf("view %s has no summary", v.ID)
		}
	}
	// Rules that could not be read at all are recorded as a revision no
	// replay can judge with.
	broken := &termsRules{problem: "could not read the outbound rules"}
	if rev := broken.revision(); rev != unreadableRulesRev {
		t.Errorf("unreadable rules recorded as %d", rev)
	}
	if rev := (*termsRules)(nil).revision(); rev != 0 {
		t.Errorf("no store recorded as %d", rev)
	}
	if _, err := (spaces.RulesTimeline{}).Rev(unreadableRulesRev); err == nil {
		t.Error("a replay judged with the unreadable revision")
	}
	// Without a store there is nothing to change.
	if _, _, err := (&server{}).changeOutboundRule(operatorCtx, method, spaces.RuleAdd, "", "command", "glab"); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no store: %v", err)
	}
}

// TestOutboundRulesPersistAndReachHook is SP-075.1.a3 (store and hook):
// rules are stored in orch.db, survive an orchd restart, and reach the
// hook's terms check through the guard snapshot; rules that do not verify
// leave the check unable to vouch for an outbound call.
func TestOutboundRulesPersistAndReachHook(t *testing.T) {
	s := spacesServer(t)
	withEnforcement(t, s)
	rig := withTermsRules(t, s)
	dir := withGuardFiles(t, s)
	applyPolicy(t, s, guardJobsPolicy)
	files := spaces.GuardFiles{Dir: dir, DB: filepath.Join(t.TempDir(), "none.db")}
	call := bashInput("sess", "/home/u/code/work", "git push")
	ctx := context.Background()
	if snap := readSnapshot(t, dir); snap.Rules != nil || snap.RulesRev != 0 || snap.RulesBroken {
		t.Fatalf("baseline snapshot: %+v", snap)
	}
	const method = "/orchd.v1.Orchd/ChangeOutboundRule"
	for _, c := range [][3]string{{"disable", "plain_variables", ""}, {"add", "fail_closed", "deploy-prod"}, {"add", "destination", "acme/public-sdk"}} {
		id, kind, value := c[1], "", ""
		if c[0] == "add" {
			id, kind, value = "", c[1], c[2]
		}
		if _, _, err := s.changeOutboundRule(operatorCtx, method, spaces.RuleAction(c[0]), id, kind, value); err != nil {
			t.Fatal(err)
		}
	}
	want := spaces.OutboundRules{Off: []spaces.OutboundRule{spaces.OutboundPlainVariables}, LastID: 2, User: []spaces.UserRule{
		{ID: "user-1", Kind: spaces.UserRuleFailClosed, Value: "deploy-prod"},
		{ID: "user-2", Kind: spaces.UserRuleDestination, Value: "acme/public-sdk"},
	}}
	snap := readSnapshot(t, dir)
	if snap.RulesRev != 3 || snap.Rules == nil || !sameRules(*snap.Rules, want) {
		t.Fatalf("snapshot rules: rev %d %+v", snap.RulesRev, snap.Rules)
	}
	if g := files.Decide(ctx, call).Terms; !g.Known || g.Rules == nil || !sameRules(*g.Rules, want) {
		t.Fatalf("hook's terms inputs: %+v", g)
	}

	// orchd restarts: the rules come back from orch.db, and the new
	// snapshot carries them.
	s2 := &server{spaces: s.spaces}
	withEnforcement(t, s2)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s2.termsRules = newTermsRules(ctx, conn)
	if cur, problem := s2.termsRules.current(); problem != "" || cur.Rev != 3 || !sameRules(cur.Rules, want) {
		t.Fatalf("after restart: %+v %q", cur, problem)
	}
	dir2 := withGuardFiles(t, s2)
	if snap := readSnapshot(t, dir2); snap.RulesRev != 3 || !sameRules(*snap.Rules, want) {
		t.Fatalf("snapshot after restart: %+v", snap)
	}

	// Rules changed in orch.db outside orchd do not verify: orchd starts
	// with them broken, and the hook cannot vouch for outbound calls.
	logs := captureLog(t)
	if _, err := conn.Exec(`UPDATE spaces_terms_rules SET rules = '{"off":["fail_closed_unreadable"]}' WHERE rev = 3`); err != nil {
		t.Fatal(err)
	}
	s3 := &server{spaces: s.spaces}
	withEnforcement(t, s3)
	s3.termsRules = newTermsRules(ctx, conn)
	if _, problem := s3.termsRules.current(); problem == "" {
		t.Fatal("tampered rules verified")
	}
	dir3 := withGuardFiles(t, s3)
	if snap := readSnapshot(t, dir3); !snap.RulesBroken || snap.Rules != nil {
		t.Fatalf("snapshot of broken rules: %+v", snap)
	}
	files3 := spaces.GuardFiles{Dir: dir3, DB: files.DB}
	if g := files3.Decide(ctx, call).Terms; !g.RulesBroken || g.Rules != nil {
		t.Fatalf("broken rules not passed to the hook: %+v", g)
	}
	assertNoRuleValues(t, "orchd's log", logs.String())
	// The operator's next change replaces the broken rules, starting from
	// the baseline.
	if _, changed, err := s3.changeOutboundRule(operatorCtx, method, spaces.RuleEnable, "plain_variables", "", ""); err != nil || !changed {
		t.Fatalf("change over broken rules: %v %v", changed, err)
	}
	if snap := readSnapshot(t, dir3); snap.RulesBroken || snap.RulesRev != 4 || len(snap.Rules.User) != 0 {
		t.Fatalf("snapshot after the fix: %+v", snap)
	}
	_ = rig
}

func sameRules(a, b spaces.OutboundRules) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// TestReplayJudgesWithRecordedRules is SP-075.1.a3 (replay, D-66): each
// command log record stores the outbound rules revision it was judged
// under, and a replay judges with that revision, not today's rules, even
// after the rules change.
func TestReplayJudgesWithRecordedRules(t *testing.T) {
	r := newReplayRig(t)
	rig := withTermsRules(t, r.s)
	// The rules apply only while a space has terms.
	terms := filepath.Join(t.TempDir(), "globex-terms.txt")
	if err := os.WriteFile(terms, []byte("zephyr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var p spaces.Policy
	if err := json.Unmarshal([]byte(replayPolicy(r.code, spaces.Isolated)), &p); err != nil {
		t.Fatal(err)
	}
	p.Spaces[3].TermsFile = terms
	doc, _ := json.Marshal(p)
	applyPolicy(t, r.s, string(doc))
	ctx := pinnedCtx(t, r.s, "sess-acme")
	const method = "/orchd.v1.Orchd/ChangeOutboundRule"
	const cmd = "deploy-prod --now"
	change := func(action spaces.RuleAction, id, kind, value string) {
		t.Helper()
		if _, _, err := r.s.changeOutboundRule(operatorCtx, method, action, id, kind, value); err != nil {
			t.Fatal(err)
		}
	}
	record := func() {
		t.Helper()
		if _, err := r.s.RecordToolEvent(ctx, bashReq("sess-acme", r.acme, cmd)); err != nil {
			t.Fatal(err)
		}
	}
	record()                                                 // revision 0, the baseline
	change(spaces.RuleAdd, "", "fail_closed", "deploy-prod") // revision 1
	record()
	change(spaces.RuleRemove, "user-1", "", "") // revision 2
	record()
	flushToolLog(t, r.s)
	recs := records(t, r.s, sessionOwner("sess-acme"))
	if len(recs) != 3 || recs[0].RulesRev != 0 || recs[1].RulesRev != 1 || recs[2].RulesRev != 2 {
		t.Fatalf("records' rules revisions: %+v", recs)
	}
	for _, rec := range recs {
		assertNoRuleValues(t, "record rule", rec.Rule)
	}

	check := func(when string) *orchdv1.ReplaySpacesResponse {
		t.Helper()
		rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
		rows := rep.GetRows()
		if len(rows) != 3 {
			t.Fatalf("%s: rows %v", when, rows)
		}
		// Only the call recorded under revision 1 is refused by the rule.
		for i, row := range rows {
			refused := row.GetRule() == string(spaces.RuleOutboundUnchecked)
			if refused != (i == 1) || !refused && row.GetRule() != row.GetRecordedRule() {
				t.Errorf("%s: row %d: %v", when, i, row)
			}
		}
		return rep
	}
	check("rule removed")
	// The rule is back (revision 3): today's rules would refuse every
	// call, but each is judged under the revision it was recorded with.
	change(spaces.RuleAdd, "", "fail_closed", "deploy-prod")
	check("rule added again")

	// A revision that does not verify makes its events problems.
	if _, err := rig.t.db.Exec(`UPDATE spaces_terms_rules SET hash = 'x' WHERE rev = 1`); err != nil {
		t.Fatal(err)
	}
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if rep.GetProblems() != 1 || !strings.Contains(rep.GetRows()[1].GetProblem(), "outbound rules") {
		t.Errorf("broken rules replay: %v", rep)
	}
	for _, row := range rep.GetRows() {
		assertNoRuleValues(t, "replay row", row.GetProblem()+row.GetRule())
	}
}
