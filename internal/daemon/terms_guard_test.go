package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// TestGuardSnapshotCarriesTermsSwitch is RC-116: the snapshot orchd writes
// for the hook carries the terms layer's switch with the guard's, so the
// hook's terms check reads it from there; a timed switch carries its end.
func TestGuardSnapshotCarriesTermsSwitch(t *testing.T) {
	s := spacesServer(t)
	rig := withEnforcement(t, s)
	dir := withGuardFiles(t, s)
	applyPolicy(t, s, guardJobsPolicy)
	files := spaces.GuardFiles{Dir: dir, DB: filepath.Join(t.TempDir(), "none.db"), Now: rig.clock.now}
	call := bashInput("sess", "/home/u/code/work", "git push")
	ctx := context.Background()
	if snap := readSnapshot(t, dir); snap.Terms.State != spaces.SwitchOn {
		t.Fatalf("snapshot terms switch at start: %+v", snap.Terms)
	}
	if g := files.Decide(ctx, call).Terms; !g.Known || g.State != spaces.SwitchOn || !g.Policy.Configured() || g.Home != "/home/u" {
		t.Fatalf("terms inputs: %+v", g)
	}
	setSwitch(t, s, string(spaces.LayerTerms), spaces.SwitchOff, 900)
	snap := readSnapshot(t, dir)
	if snap.Terms.State != spaces.SwitchOff || snap.Terms.Until.IsZero() || snap.Terms.Previous != spaces.SwitchOn {
		t.Fatalf("snapshot after a timed off: %+v", snap.Terms)
	}
	if g := files.Decide(ctx, call).Terms; g.State != spaces.SwitchOff {
		t.Fatalf("terms state while off: %q", g.State)
	}
	rig.clock.advance(15 * time.Minute)
	if g := files.Decide(ctx, call).Terms; g.State != spaces.SwitchOn {
		t.Fatalf("terms state after the timer: %q", g.State)
	}
	// Switching the guard keeps the terms layer's switch.
	setSwitch(t, s, string(spaces.LayerTerms), spaces.SwitchOff, 0)
	setSwitch(t, s, string(spaces.LayerGuard), spaces.SwitchLog, 0)
	if snap := readSnapshot(t, dir); snap.Terms.State != spaces.SwitchOff || snap.Guard.State != spaces.SwitchLog {
		t.Fatalf("snapshot: guard %+v terms %+v", snap.Guard, snap.Terms)
	}
}

// TestTermsRefusalRecordedAndReplayed is RC-116 through the command log
// and replay (SP-047): a call the hook's terms check refused is stored as
// refused with its rule, like a guard refusal, and a replay carries the
// verdict while the replayed guard lets the call through and the terms
// layer is on; with the layer off, or a guard that now refuses first, the
// replay reports the change.
func TestTermsRefusalRecordedAndReplayed(t *testing.T) {
	r := newReplayRig(t)
	ctx := pinnedCtx(t, r.s, "sess-acme")
	refused := func(cmd string, rule spaces.GuardRule) *orchdv1.RecordToolEventRequest {
		req := bashReq("sess-acme", r.acme, cmd)
		req.GuardVerdict, req.GuardRule, req.GuardEnforced, req.GuardPolicyRev = "refuse", string(rule), true, 1
		return req
	}
	for _, req := range []*orchdv1.RecordToolEventRequest{
		refused(`git commit -m "notes"`, spaces.RuleOutboundTerm),
		refused(`git commit -m "$(date)"`, spaces.RuleOutboundUnchecked),
		refused(`cat `+r.globex+`/notes && gh issue create -R acme/app -b x`, spaces.RuleOutboundTerm),
	} {
		if _, err := r.s.RecordToolEvent(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	flushToolLog(t, r.s)
	recs := records(t, r.s, sessionOwner("sess-acme"))
	want := []spaces.GuardRule{spaces.RuleOutboundTerm, spaces.RuleOutboundUnchecked, spaces.RuleOutboundTerm}
	if len(recs) != len(want) {
		t.Fatalf("records %d", len(recs))
	}
	for i, rec := range recs {
		if rec.Decision != toollog.DecisionRefused || rec.Rule != string(want[i]) {
			t.Errorf("record %d: %q %q", i, rec.Decision, rec.Rule)
		}
	}
	rows := func(rep *orchdv1.ReplaySpacesResponse) []*orchdv1.ReplayedEvent { return rep.GetRows() }
	// Logging guard: every terms verdict is carried.
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if rep.GetCompared() != 3 || rep.GetChanged() != 0 {
		t.Fatalf("replay = %v", rep)
	}
	for i, row := range rows(rep) {
		if row.GetVerdict() != string(spaces.Refuse) || row.GetRule() != string(want[i]) {
			t.Errorf("row %d: %v", i, row)
		}
	}
	// An enforcing guard refuses the third call (it reads another job's
	// folder) before the terms check would run.
	rep = replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true, Mode: spaces.SwitchBlockOtherJobs})
	if rep.GetChanged() != 1 || rows(rep)[2].GetRule() != string(spaces.RuleOtherJobPath) || rows(rep)[0].GetRule() != string(spaces.RuleOutboundTerm) {
		t.Fatalf("block-other-jobs replay = %v", rep)
	}
	// With the terms layer off, no terms verdict is carried.
	setSwitch(t, r.s, string(spaces.LayerTerms), spaces.SwitchOff, 0)
	rep = replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if rep.GetChanged() != 3 {
		t.Fatalf("terms off replay = %v", rep)
	}
	for _, row := range rows(rep) {
		if row.GetRule() == string(spaces.RuleOutboundTerm) || row.GetRule() == string(spaces.RuleOutboundUnchecked) {
			t.Errorf("terms verdict carried with the layer off: %v", row)
		}
	}
}
