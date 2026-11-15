package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// fakeMover records the switch's store moves. A move blocks while gate is
// set until it is closed, and fails with the next error in errs.
type fakeMover struct {
	mu     sync.Mutex
	calls  []string
	errs   []error
	gate   chan struct{}
	inMove chan string
}

func (m *fakeMover) move(ctx context.Context, dir string, progress func(rag.MoveReport)) (rag.MoveReport, error) {
	m.mu.Lock()
	m.calls = append(m.calls, dir)
	gate, in := m.gate, m.inMove
	var err error
	if len(m.errs) > 0 {
		err, m.errs = m.errs[0], m.errs[1:]
	}
	m.mu.Unlock()
	rep := rag.MoveReport{Direction: dir, Collections: 1}
	progress(rep)
	if in != nil {
		in <- dir
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return rep, ctx.Err()
		}
	}
	return rep, err
}

func (m *fakeMover) MergeSpaces(ctx context.Context, p func(rag.MoveReport)) (rag.MoveReport, error) {
	return m.move(ctx, rag.MoveMerge, p)
}

func (m *fakeMover) SplitSpaces(ctx context.Context, p func(rag.MoveReport)) (rag.MoveReport, error) {
	return m.move(ctx, rag.MoveSplit, p)
}

func (m *fakeMover) called() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// appliedEngine is a spaces engine with a policy applied.
func appliedEngine(t *testing.T) *spaces.Engine {
	t.Helper()
	s := spacesServer(t)
	d := draft(t, s, spacesDoc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	return s.spaces
}

// testSwitch is a switch over an applied engine and m, its marker in a
// temporary directory, counting notifications and relabels.
func testSwitch(t *testing.T, m *fakeMover) (w *spacesSwitch, notified, enforced *int) {
	t.Helper()
	w = newSpacesSwitch(appliedEngine(t), m, filepath.Join(t.TempDir(), "migrations", "spaces-split.json"))
	var mu sync.Mutex
	n, e := 0, 0
	w.applied = func() { mu.Lock(); n++; mu.Unlock() }
	w.enforce = func(context.Context) error { mu.Lock(); e++; mu.Unlock(); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); w.stop() })
	w.ctx = ctx
	return w, &n, &e
}

func ok() error { return nil }

// TestSpacesSwitchStartsOff: off (the default), the engine enforces nothing
// from the start and the store is merged; the policy is kept.
func TestSpacesSwitchStartsOff(t *testing.T) {
	m := &fakeMover{}
	w, _, _ := testSwitch(t, m)
	w.prepare(false)
	if !w.engine.Off() {
		t.Fatal("engine enforces with spaces off")
	}
	w.start(w.ctx)
	w.wait()
	if got := m.called(); len(got) != 1 || got[0] != rag.MoveMerge {
		t.Fatalf("moves = %v, want one merge", got)
	}
	if !w.splitPending() {
		t.Fatal("a merge ran without recording the split it makes owed")
	}
	if st, _ := w.state(); st != spacesStateOff {
		t.Fatalf("state = %q", st)
	}
	if st, err := w.engine.Status(context.Background()); err != nil || st.AppliedRevision == 0 {
		t.Fatalf("the applied policy was not kept: %+v, %v", st, err)
	}
}

// TestSpacesSwitchOnSplitsBeforeEnforcing: turning on records the split it
// owes, keeps enforcement off until the split ends, then enforces, relabels
// and forgets the marker.
func TestSpacesSwitchOnSplitsBeforeEnforcing(t *testing.T) {
	m := &fakeMover{gate: make(chan struct{}), inMove: make(chan string, 4)}
	w, notified, enforced := testSwitch(t, m)
	w.prepare(false)
	stored := false
	if err := w.change(true, "the operator", func() error {
		if !w.splitPending() {
			t.Error("spaces.enabled was saved before the pending split was recorded")
		}
		stored = true
		return nil
	}); err != nil || !stored {
		t.Fatalf("change = %v, stored %v", err, stored)
	}
	if dir := <-m.inMove; dir != rag.MoveSplit {
		t.Fatalf("move = %s", dir)
	}
	if !w.engine.Off() {
		t.Fatal("enforcing before the split ended")
	}
	if st, mv := w.state(); st != spacesStateTurningOn || mv == nil || !mv.GetRunning() || mv.GetDirection() != rag.MoveSplit {
		t.Fatalf("state while splitting = %q, %v", st, mv)
	}
	if !w.off() || w.wantsOff() {
		t.Fatal("off()/wantsOff() while turning on")
	}
	close(m.gate)
	w.wait()
	if w.engine.Off() || w.splitPending() {
		t.Fatalf("after the split: engine off %v, marker kept %v", w.engine.Off(), w.splitPending())
	}
	if *notified != 1 || *enforced != 1 {
		t.Fatalf("notified %d, relabelled %d; want 1 each", *notified, *enforced)
	}
	if st, mv := w.state(); st != spacesStateOn || mv != nil {
		t.Fatalf("state once on = %q, %v", st, mv)
	}
}

// TestSpacesSwitchResumesASplitAfterARestart: the marker of a split owed
// keeps enforcement off at the next start until the split finishes.
func TestSpacesSwitchResumesASplitAfterARestart(t *testing.T) {
	m := &fakeMover{}
	w, _, _ := testSwitch(t, m)
	if err := w.writeMarker(); err != nil {
		t.Fatal(err)
	}
	w.prepare(true)
	if !w.engine.Off() {
		t.Fatal("enforcing with a split owed")
	}
	w.start(w.ctx)
	w.wait()
	if got := m.called(); len(got) != 1 || got[0] != rag.MoveSplit || w.engine.Off() || w.splitPending() {
		t.Fatalf("moves %v, engine off %v, marker %v", got, w.engine.Off(), w.splitPending())
	}
	// Without a marker, on enforces at once and moves nothing.
	m2 := &fakeMover{}
	w2, _, _ := testSwitch(t, m2)
	w2.prepare(true)
	w2.start(w2.ctx)
	w2.wait()
	if w2.engine.Off() || len(m2.called()) != 0 {
		t.Fatalf("engine off %v, moves %v", w2.engine.Off(), m2.called())
	}
}

// TestSpacesSwitchOffDuringASplit: switching off while a split runs stops
// it, stops enforcing, merges and forgets the split owed.
func TestSpacesSwitchOffDuringASplit(t *testing.T) {
	m := &fakeMover{gate: make(chan struct{}), inMove: make(chan string, 4)}
	w, _, enforced := testSwitch(t, m)
	w.prepare(false)
	if err := w.change(true, "the operator", ok); err != nil {
		t.Fatal(err)
	}
	<-m.inMove
	m.mu.Lock()
	m.gate = nil
	m.mu.Unlock()
	if err := w.change(false, "the operator", ok); err != nil {
		t.Fatal(err)
	}
	w.wait()
	if got := m.called(); len(got) != 2 || got[1] != rag.MoveMerge {
		t.Fatalf("moves = %v", got)
	}
	// The merged store keeps the marker: a split is owed before anything
	// is enforced again.
	if !w.engine.Off() || !w.splitPending() || *enforced != 0 {
		t.Fatalf("engine off %v, marker %v, relabelled %d", w.engine.Off(), w.splitPending(), *enforced)
	}
}

// TestSpacesSwitchKeepsTheOldValueWhenSavingFails: a failed save changes
// nothing that is enforced.
func TestSpacesSwitchKeepsTheOldValueWhenSavingFails(t *testing.T) {
	w, _, _ := testSwitch(t, &fakeMover{})
	w.prepare(true)
	if err := w.change(false, "the operator", func() error { return errors.New("locked") }); err == nil {
		t.Fatal("a failed save was not reported")
	}
	if w.engine.Off() || w.wantsOff() {
		t.Fatal("a failed save switched spaces off")
	}
}

// TestSpacesSwitchRetriesAFailedMove: a move that fails runs again.
func TestSpacesSwitchRetriesAFailedMove(t *testing.T) {
	old := moveRetry
	moveRetry = time.Millisecond
	t.Cleanup(func() { moveRetry = old })
	m := &fakeMover{errs: []error{errors.New("qdrant down")}}
	w, _, _ := testSwitch(t, m)
	w.prepare(false)
	w.start(w.ctx)
	w.wait()
	if got := m.called(); len(got) != 2 {
		t.Fatalf("moves = %v, want a failed merge then one that succeeded", got)
	}
	if _, mv := w.state(); mv != nil {
		t.Fatalf("a finished move is still reported: %v", mv)
	}
}

// TestSpacesSwitchWaitsForTheMigrationLock: a move does not run while a
// migration or relabel holds the lock.
func TestSpacesSwitchWaitsForTheMigrationLock(t *testing.T) {
	w, _, _ := testSwitch(t, &fakeMover{})
	migrating.Lock()
	err := w.once(context.Background(), rag.MoveMerge)
	migrating.Unlock()
	if !errors.Is(err, errMoveBusy) {
		t.Fatalf("once while locked = %v", err)
	}
}

// TestSpacesOffGate is the RPC half of "disabled = pre-spaces behaviour":
// while off, the spaces RPCs that change enforcement are refused, the hooks'
// calls are dropped, and everything else runs; on, everything runs.
func TestSpacesOffGate(t *testing.T) {
	w, _, _ := testSwitch(t, &fakeMover{})
	gate := spacesOffGate(w)
	call := func(method string) (bool, error) {
		ran := false
		_, err := gate(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) {
			ran = true
			return nil, nil
		})
		return ran, err
	}
	w.prepare(false)
	for m := range spacesOffRefused {
		if ran, err := call(m); ran || grpcstatus.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "spaces are off") {
			t.Errorf("%s while off: ran %v, %v", m, ran, err)
		}
	}
	for m := range spacesOffDropped {
		if ran, err := call(m); ran || err != nil {
			t.Errorf("%s while off: ran %v, %v", m, ran, err)
		}
	}
	for _, m := range []string{orchdv1.Orchd_Search_FullMethodName, orchdv1.Orchd_ListSpaces_FullMethodName, orchdv1.Orchd_SetConfig_FullMethodName} {
		if ran, err := call(m); !ran || err != nil {
			t.Errorf("%s while off: ran %v, %v", m, ran, err)
		}
	}
	w.prepare(true)
	for _, m := range []string{orchdv1.Orchd_ApplySpaces_FullMethodName, orchdv1.Orchd_PinSession_FullMethodName} {
		if ran, err := call(m); !ran || err != nil {
			t.Errorf("%s while on: ran %v, %v", m, ran, err)
		}
	}
	if ran, err := spacesOffGate(nil)(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: orchdv1.Orchd_ApplySpaces_FullMethodName},
		func(context.Context, any) (any, error) { return nil, nil }); ran != nil || err != nil {
		t.Errorf("nil switch: %v, %v", ran, err)
	}
}

// TestSetConfigSpacesSwitch: spaces.enabled is changed only by the
// operator, confirmed, on its own, and applied at once.
func TestSetConfigSpacesSwitch(t *testing.T) {
	c := configSvc(t)
	e, err := spaces.Open(context.Background(), c.db, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMover{}
	w := newSpacesSwitch(e, m, filepath.Join(t.TempDir(), "spaces-split.json"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); w.stop() })
	w.ctx = ctx
	w.prepare(false)
	on := map[string]string{config.KeySpacesEnabled: "true"}
	harness := callerCtx(callerAuth{pid: 7, harness: "claude"})
	cases := []struct {
		name    string
		ctx     context.Context
		values  map[string]string
		confirm bool
		sw      *spacesSwitch
		want    codes.Code
	}{
		{"an agent session", harness, on, true, w, codes.PermissionDenied},
		{"with another key", operatorCtx, map[string]string{config.KeySpacesEnabled: "true", "docs.include_forks": "true"}, true, w, codes.InvalidArgument},
		{"unconfirmed", operatorCtx, on, false, w, codes.FailedPrecondition},
		{"settings only", operatorCtx, on, true, nil, codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.spaces = tc.sw
			_, err := c.set(tc.ctx, &orchdv1.SetConfigRequest{Values: tc.values, ConfirmSensitive: tc.confirm})
			if grpcstatus.Code(err) != tc.want {
				t.Fatalf("set = %v, want %v", err, tc.want)
			}
			if on, _ := spacesEnabledStored(t, c); on {
				t.Fatal("refused, yet stored")
			}
		})
	}
	c.spaces = w
	st, err := c.set(operatorCtx, &orchdv1.SetConfigRequest{Values: on, ConfirmSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if f := configField(t, st, config.KeySpacesEnabled); f.GetValue() != "true" || f.GetPendingRestart() {
		t.Fatalf("field after switching on = %v", f)
	}
	w.wait()
	if got := m.called(); len(got) != 1 || got[0] != rag.MoveSplit || w.engine.Off() {
		t.Fatalf("moves %v, engine off %v", got, w.engine.Off())
	}
	if _, err := c.set(operatorCtx, &orchdv1.SetConfigRequest{Values: map[string]string{config.KeySpacesEnabled: "false"}, ConfirmSensitive: true}); err != nil {
		t.Fatal(err)
	}
	if !w.engine.Off() {
		t.Fatal("switching off did not stop enforcement")
	}
	if on, _ := spacesEnabledStored(t, c); on {
		t.Fatal("off was not stored")
	}
	// A store that cannot be written changes nothing that is enforced.
	_ = c.db.Close()
	if _, err := c.set(operatorCtx, &orchdv1.SetConfigRequest{Values: on, ConfirmSensitive: true}); grpcstatus.Code(err) != codes.Internal {
		t.Fatalf("set with a closed store = %v", err)
	}
	if !w.engine.Off() {
		t.Fatal("a failed save turned enforcement on")
	}
}

func spacesEnabledStored(t *testing.T, c *configService) (bool, error) {
	t.Helper()
	v, err := config.LoadStore(context.Background(), c.db)
	if err != nil {
		t.Fatal(err)
	}
	return v.GetBool(config.KeySpacesEnabled), nil
}

// TestGuardSnapshotCarriesSpacesOff: while off orchd writes no policy and
// marks the snapshot, so the hooks do nothing; on, the policy is back.
func TestGuardSnapshotCarriesSpacesOff(t *testing.T) {
	e := appliedEngine(t)
	dir := t.TempDir()
	s := &server{spaces: e, guard: newGuardWriter(dir, "/home/u", "/c", "/d", "/b")}
	e.SetOff(true)
	s.writeGuardSnapshot()
	snap, err := spaces.ReadGuardSnapshot(dir)
	if err != nil || !snap.SpacesOff || snap.Policy != nil || snap.Revision != 0 {
		t.Fatalf("snapshot while off = %+v, %v", snap, err)
	}
	e.SetOff(false)
	s.writeGuardSnapshot()
	if snap, err = spaces.ReadGuardSnapshot(dir); err != nil || snap.SpacesOff || snap.Policy == nil {
		t.Fatalf("snapshot once on = %+v, %v", snap, err)
	}
}

// TestSpacesStatusReportsTheSwitch: Status names the switch and the move.
func TestSpacesStatusReportsTheSwitch(t *testing.T) {
	w, _, _ := testSwitch(t, &fakeMover{})
	w.prepare(false)
	w.setMove(&orchdv1.SpacesStoreMove{Direction: rag.MoveMerge, Running: true})
	s := &server{spaces: w.engine, spacesSwitch: w}
	st := s.spacesStatus(context.Background())
	if st.GetState() != spacesStateOff || st.GetStoreMove().GetDirection() != rag.MoveMerge || st.GetConfigured() || st.GetAppliedRevision() == 0 {
		t.Fatalf("status = %v", st)
	}
	var none *spacesSwitch
	if got, mv := none.state(); got != "" || mv != nil || none.off() || none.wantsOff() {
		t.Fatal("a nil switch reports a state")
	}
}

// TestMoveMarkerFile: the marker is owner-only JSON beside the migrations.
func TestMoveMarkerFile(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	w := newSpacesSwitch(nil, nil, defaultMoveMarker(paths.DataDir()))
	if err := w.writeMarker(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(w.markerPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker = %v, %v", info, err)
	}
	b, _ := os.ReadFile(w.markerPath)
	var mk moveMarker
	if err := json.Unmarshal(b, &mk); err != nil || mk.Direction != rag.MoveSplit {
		t.Fatalf("marker = %s, %v", b, err)
	}
	w.removeMarker()
	w.removeMarker()
	if w.splitPending() {
		t.Fatal("marker kept")
	}
	empty := newSpacesSwitch(nil, nil, "")
	if empty.splitPending() || empty.writeMarker() != nil {
		t.Fatal("a switch without a marker path keeps one")
	}
	empty.removeMarker()
	empty.run(rag.MoveMerge) // no run context: runs nothing
	empty.wait()
	if err := empty.once(context.Background(), rag.MoveMerge); err != nil {
		t.Fatal(err)
	}
}

// TestSpacesSwitchAtStart: a fresh store starts off; one that applied a
// policy before the switch existed starts on and stores it; the operator's
// stored off wins; a store that cannot be read keeps enforcing.
func TestSpacesSwitchAtStart(t *testing.T) {
	ctx := context.Background()
	c := configSvc(t)
	if spacesSwitchAtStart(ctx, c.db) {
		t.Fatal("a fresh store starts with spaces on")
	}
	s := spacesServer(t)
	d := draft(t, s, spacesDoc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	store, err := dbOpenForTest()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if !spacesSwitchAtStart(ctx, store) {
		t.Fatal("a store with an applied policy starts with spaces off")
	}
	if on, stored, err := config.SpacesEnabledIn(ctx, store); err != nil || !on || !stored {
		t.Fatalf("after start: %v, %v, %v; want on, stored", on, stored, err)
	}
	if err := config.ApplyStore(ctx, store, map[string]any{config.KeySpacesEnabled: false}, true, "operator"); err != nil {
		t.Fatal(err)
	}
	if spacesSwitchAtStart(ctx, store) {
		t.Fatal("the operator's off was not kept")
	}
	_ = store.Close()
	if !spacesSwitchAtStart(ctx, store) {
		t.Fatal("an unreadable store turned spaces off")
	}
}

// dbOpenForTest opens orch.db in the test's data directory.
func dbOpenForTest() (*sql.DB, error) { return db.Open() }

// TestSpacesSwitchOnAgainChangesNothing: switching on while on and
// enforcing stores the value and moves nothing, so a restart cannot find a
// split owed and stop enforcing.
func TestSpacesSwitchOnAgainChangesNothing(t *testing.T) {
	m := &fakeMover{}
	w, _, _ := testSwitch(t, m)
	w.prepare(true)
	stored := false
	if err := w.change(true, "the operator", func() error { stored = true; return nil }); err != nil || !stored {
		t.Fatalf("change = %v, stored %v", err, stored)
	}
	w.wait()
	if len(m.called()) != 0 || w.splitPending() || w.engine.Off() {
		t.Fatalf("moves %v, marker %v, engine off %v", m.called(), w.splitPending(), w.engine.Off())
	}
}

// failingSplit reports a chunk it could not move, then succeeds.
type failingSplit struct {
	fakeMover
	n int
}

func (f *failingSplit) SplitSpaces(ctx context.Context, p func(rag.MoveReport)) (rag.MoveReport, error) {
	rep, err := f.move(ctx, rag.MoveSplit, p)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == 1 {
		rep.Failed = 1
	}
	return rep, err
}

// TestSpacesSwitchWaitsForAWholeSplit: a split that left a chunk in the base
// keeps enforcement off, reports it, and runs again.
func TestSpacesSwitchWaitsForAWholeSplit(t *testing.T) {
	old := moveRetry
	moveRetry = 50 * time.Millisecond
	t.Cleanup(func() { moveRetry = old })
	m := &failingSplit{}
	w, _, _ := testSwitch(t, &fakeMover{})
	w.mover = m
	w.prepare(false)
	if err := w.change(true, "the operator", ok); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, mv := w.state(); mv != nil && strings.Contains(mv.GetError(), "could not be moved") {
			if !w.engine.Off() {
				t.Fatal("enforcing with chunks left in the base")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the incomplete split was not reported")
		}
		time.Sleep(time.Millisecond)
	}
	w.wait()
	if w.engine.Off() || w.splitPending() {
		t.Fatalf("after the retry: engine off %v, marker %v", w.engine.Off(), w.splitPending())
	}
}

// TestSpacesSwitchRetriesTheRelabel: a relabel that fails after the split
// runs again.
func TestSpacesSwitchRetriesTheRelabel(t *testing.T) {
	old := moveRetry
	moveRetry = time.Millisecond
	t.Cleanup(func() { moveRetry = old })
	w, _, _ := testSwitch(t, &fakeMover{})
	calls := 0
	w.enforce = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("a migration is running")
		}
		return nil
	}
	w.prepare(false)
	if err := w.change(true, "the operator", ok); err != nil {
		t.Fatal(err)
	}
	w.wait()
	if calls != 2 {
		t.Fatalf("relabel ran %d time(s), want 2", calls)
	}
}

// TestSpacesSwitchDoesNotMergeWithoutAMarker: a merge never runs when the
// record of the split it makes owed cannot be written.
func TestSpacesSwitchDoesNotMergeWithoutAMarker(t *testing.T) {
	m := &fakeMover{}
	w, _, _ := testSwitch(t, m)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.markerPath = filepath.Join(blocker, "spaces-split.json")
	w.prepare(false)
	w.start(w.ctx)
	w.wait()
	if len(m.called()) != 0 {
		t.Fatalf("moves = %v, want none", m.called())
	}
	if err := w.change(true, "the operator", ok); err == nil {
		t.Fatal("switching on without the marker was not refused")
	}
}
