package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeHooks records the tool hook settings changes the hooks switch makes,
// and lists rows.
type fakeHooks struct {
	mu        sync.Mutex
	calls     []string
	err       error
	rows      []settings.Row
	providers map[string]settings.ProviderState
	listErr   error
}

func (f *fakeHooks) List(context.Context, string, string) (settings.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return settings.Listing{Rows: f.rows, Providers: f.providers}, f.listErr
}

func (f *fakeHooks) SetEnabled(_ context.Context, provider, kind, identity string, enabled bool) (settings.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "off"
	if enabled {
		state = "on"
	}
	f.calls = append(f.calls, provider+"/"+kind+"/"+identity+"="+state)
	return settings.Row{}, f.err
}

// switchRig is a server with enforcement switches on a fake clock, its
// emitted changes and the hook settings it changed.
type switchRig struct {
	s       *server
	clock   *fakeClock
	hooks   *fakeHooks
	emitted *[]spaces.SwitchChange
	notes   *[][]string
}

// withEnforcement attaches switches in the test's orch.db (opened again,
// as spacesServer leaves its own connection inside the engine).
func withEnforcement(t *testing.T, s *server) switchRig {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	clock := &fakeClock{t: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)}
	hooks := &fakeHooks{}
	var mu sync.Mutex
	emitted := &[]spaces.SwitchChange{}
	notes := &[][]string{}
	e := newEnforcement(context.Background(), conn, nil)
	e.now, e.hooks = clock.now, hooks
	e.emit = func(c spaces.SwitchChange, n []string) {
		mu.Lock()
		defer mu.Unlock()
		*emitted = append(*emitted, c)
		*notes = append(*notes, n)
	}
	s.enforcement = e
	return switchRig{s: s, clock: clock, hooks: hooks, emitted: emitted, notes: notes}
}

func setSwitch(t *testing.T, s *server, layer, state string, forSecs int64) *orchdv1.SetEnforcementResponse {
	t.Helper()
	rep, err := s.SetEnforcement(operatorCtx, &orchdv1.SetEnforcementRequest{Layer: layer, State: state, ForSeconds: forSecs})
	if err != nil {
		t.Fatalf("SetEnforcement %s %s: %v", layer, state, err)
	}
	return rep
}

func switchesOf(t *testing.T, s *server, ctx context.Context) map[string]*orchdv1.EnforcementSwitch {
	t.Helper()
	rep, err := s.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*orchdv1.EnforcementSwitch{}
	for _, sw := range rep.GetSwitches() {
		out[sw.GetLayer()] = sw
	}
	return out
}

// TestSetEnforcementEveryLayerState is SP-070.1.a1 through the RPC: every
// layer can be set to each of its states, and the defaults are memory on,
// guard block-other-jobs, terms on, sandbox on (and hooks on).
func TestSetEnforcementEveryLayerState(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	want := map[string]string{"memory": "on", "guard": "block-other-jobs", "terms": "on", "sandbox": "on", "hooks": "on"}
	got := switchesOf(t, r.s, operatorCtx)
	if len(got) != len(want) {
		t.Fatalf("%d layers listed, want %d", len(got), len(want))
	}
	for l, st := range want {
		if got[l].GetState() != st || got[l].GetDefaultState() != st || got[l].GetRelaxed() || got[l].GetSetAt() != 0 {
			t.Errorf("default %s = %v, want %s", l, got[l], st)
		}
	}
	cases := 0
	for _, l := range spaces.Layers {
		for _, st := range append(l.States()[1:], l.Default()) {
			rep := setSwitch(t, r.s, string(l), st, 0)
			if !rep.GetChanged() || rep.GetCurrent().GetState() != st {
				t.Errorf("%s -> %s: %v", l, st, rep)
			}
			if sw := switchesOf(t, r.s, operatorCtx)[string(l)]; sw.GetState() != st || sw.GetRelaxed() != l.Relaxed(st) {
				t.Errorf("after %s -> %s, GetEnforcement says %v", l, st, sw)
			}
			cases++
		}
	}
	if cases != 12 {
		t.Errorf("%d layer x state cases, want 12", cases)
	}
	if rep := setSwitch(t, r.s, "memory", "on", 0); rep.GetChanged() {
		t.Error("setting the state a layer already has reported a change")
	}
	for _, bad := range []*orchdv1.SetEnforcementRequest{
		{Layer: "firewall", State: "off"},
		{Layer: "memory", State: "strict"},
		{Layer: "guard", State: "on"},
		{Layer: "guard", State: "off", ForSeconds: -1},
		{Layer: "guard", State: "off", ForSeconds: 25 * 3600},
	} {
		if _, err := r.s.SetEnforcement(operatorCtx, bad); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("SetEnforcement(%v) = %v, want InvalidArgument", bad, err)
		}
	}
}

// TestSetEnforcementOperatorOnly is SP-070.1.a2: SetEnforcement is refused
// for callers descended from a harness or orchd, and for callers orchd
// cannot place; the switch is then unchanged.
func TestSetEnforcementOperatorOnly(t *testing.T) {
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	tests := []struct {
		name string
		ctx  context.Context
		says string
	}{
		{"an interactive claude session", callerCtx(callerAuth{pid: 7, harness: "claude"}), "inside a claude session"},
		{"an interactive codex session", callerCtx(callerAuth{pid: 7, harness: "codex"}), "inside a codex session"},
		{"a process orchd started", callerCtx(callerAuth{pid: 7, spawned: true}), "orchd started"},
		{"a queued task", queued, "task task-1"},
		{"an unreadable ancestry", callerCtx(callerAuth{pid: 7, harnessErr: errors.New("gone")}), "cannot read"},
		{"an unidentified peer", callerCtx(callerAuth{err: errPeerUnsupported}), "cannot identify"},
		{"no peer at all", context.Background(), "unidentified"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := withEnforcement(t, spacesServer(t))
			_, err := r.s.SetEnforcement(tt.ctx, &orchdv1.SetEnforcementRequest{Layer: "guard", State: "off"})
			if grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), tt.says) {
				t.Fatalf("SetEnforcement = %v, want PermissionDenied saying %q", err, tt.says)
			}
			if st := switchesOf(t, r.s, operatorCtx)["guard"].GetState(); st != spaces.SwitchBlockOtherJobs {
				t.Errorf("a refused switch changed the guard to %s", st)
			}
			if len(*r.emitted) != 0 {
				t.Errorf("a refused switch emitted %v", *r.emitted)
			}
		})
	}
	// Reading the switches is open to everyone, so every session's digest
	// can say what is off; who set one is the operator's to see.
	r := withEnforcement(t, spacesServer(t))
	setSwitch(t, r.s, "guard", "off", 0)
	for _, ctx := range []context.Context{inClaude, queued} {
		sw := switchesOf(t, r.s, ctx)["guard"]
		if sw.GetState() != "off" || sw.GetSetBy() != "" {
			t.Errorf("an agent read %v", sw)
		}
	}
	if err := refuseQueued(queued, orchdv1.Orchd_GetEnforcement_FullMethodName); err != nil {
		t.Errorf("a queued task cannot read the switches: %v", err)
	}
	if by := switchesOf(t, r.s, operatorCtx)["guard"].GetSetBy(); by != "operator (process 42)" {
		t.Errorf("set_by = %q", by)
	}
}

// TestTimedSwitchReverts is SP-070.1.a3: with a fake clock, a timed switch
// reverts to its previous state at expiry and emits an event; a timed
// switch over another keeps the first one's revert state.
func TestTimedSwitchReverts(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	ctx := context.Background()
	rep := setSwitch(t, r.s, "guard", "off", 15*60)
	if u := rep.GetCurrent().GetUntil(); u != r.clock.now().Add(15*time.Minute).Unix() || rep.GetCurrent().GetRevertsTo() != "block-other-jobs" {
		t.Fatalf("timed switch = %v", rep.GetCurrent())
	}
	r.clock.advance(15*time.Minute - time.Second)
	r.s.enforcement.expire(ctx)
	if st := r.s.enforcement.state(spaces.LayerGuard); st != "off" || len(*r.emitted) != 1 {
		t.Fatalf("before expiry: guard %s, %d events", st, len(*r.emitted))
	}
	r.clock.advance(time.Second)
	r.s.enforcement.expire(ctx)
	if st := r.s.enforcement.state(spaces.LayerGuard); st != "block-other-jobs" {
		t.Fatalf("after expiry the guard is %s, want block-other-jobs", st)
	}
	if len(*r.emitted) != 2 {
		t.Fatalf("events = %v, want the switch and its expiry", *r.emitted)
	}
	if c := (*r.emitted)[1]; !c.Expired || c.Layer != spaces.LayerGuard || c.State != "block-other-jobs" || c.Previous != "off" ||
		!c.At.Equal(r.clock.now()) || !strings.Contains(c.Describe(), "expired") {
		t.Errorf("expiry event = %+v", c)
	}
	if sw := switchesOf(t, r.s, operatorCtx)["guard"]; sw.GetUntil() != 0 || sw.GetRelaxed() {
		t.Errorf("after expiry GetEnforcement says %v", sw)
	}

	// A chain of timed switches ends where it began.
	setSwitch(t, r.s, "memory", "off", 3600)
	r.clock.advance(time.Minute)
	setSwitch(t, r.s, "memory", "on", 60)
	r.clock.advance(time.Minute)
	if st := switchesOf(t, r.s, operatorCtx)["memory"]; st.GetState() != "on" || st.GetUntil() != 0 {
		t.Errorf("memory after the chain = %v, want on and untimed", st)
	}
	// An untimed switch replaces a timer.
	setSwitch(t, r.s, "terms", "off", 60)
	setSwitch(t, r.s, "terms", "off", 0)
	r.clock.advance(time.Hour)
	if st := r.s.enforcement.state(spaces.LayerTerms); st != "off" {
		t.Errorf("an untimed switch over a timed one reverted to %s", st)
	}
}

// TestEnforcementRunRevertsOnTime: orchd's tick reverts a timed switch
// without any reader asking, so its event arrives on time.
func TestEnforcementRunRevertsOnTime(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	setSwitch(t, r.s, "sandbox", "off", 1)
	r.clock.advance(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.s.enforcement.run(ctx, time.Millisecond); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.s.enforcement.mu.Lock()
		n := len(*r.emitted)
		r.s.enforcement.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tick did not revert the timed switch")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// TestToolHooksSwitch is SP-070.1.a4: orch settings cannot turn the tool
// hooks off (or on); only the operator's hooks switch does, which turns
// Claude Code's tool hooks off in its settings, is logged with time and
// caller, and is named by every session's start digest while off.
func TestToolHooksSwitch(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	r.s.settings = &settings.Service{}
	for _, id := range toolHookIdentities {
		for _, on := range []bool{false, true} {
			_, err := r.s.SetSettingEnabled(operatorCtx, &orchdv1.SetSettingEnabledRequest{
				Provider: install.ProviderClaudeCode, Kind: install.KindHook, Identity: id, Enabled: on})
			if grpcstatus.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "orch spaces enforce hooks off") {
				t.Errorf("SetSettingEnabled(%s, %v) = %v, want refused", id, on, err)
			}
		}
	}
	if isToolHook(install.ProviderClaudeCode, install.KindHook, install.HookIdentity("SessionStart", "session-start")) {
		t.Error("an ordinary hook counts as a tool hook")
	}
	if _, err := r.s.SetEnforcement(inClaude, &orchdv1.SetEnforcementRequest{Layer: "hooks", State: "off"}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("a session switched the hooks off: %v", err)
	}
	if len(r.hooks.calls) != 0 {
		t.Fatalf("a refused switch changed settings: %v", r.hooks.calls)
	}

	setSwitch(t, r.s, "hooks", "off", 0)
	wantOff := []string{"claude-code/hook/PreToolUse/orch-hook pre-tool-use=off", "claude-code/hook/CwdChanged/orch-hook cwd-changed=off"}
	if strings.Join(r.hooks.calls, ",") != strings.Join(wantOff, ",") {
		t.Errorf("hooks off changed %v, want %v", r.hooks.calls, wantOff)
	}
	hist, err := spaces.SwitchHistory(context.Background(), r.s.enforcement.db, 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %v, %v", hist, err)
	}
	if h := hist[0]; h.Layer != spaces.LayerHooks || h.State != "off" || h.By != "operator (process 42)" || !h.At.Equal(r.clock.now()) {
		t.Errorf("logged change = %+v, want time and caller", h)
	}
	if sw := switchesOf(t, r.s, inClaude)["hooks"]; !sw.GetRelaxed() || sw.GetSetAt() != r.clock.now().Unix() {
		t.Errorf("a session sees the hooks switch as %v", sw)
	}

	r.hooks.calls = nil
	r.hooks.err = settings.ErrNotFound
	rep := setSwitch(t, r.s, "hooks", "on", 0)
	if len(r.hooks.calls) != 2 || !strings.HasSuffix(r.hooks.calls[0], "=on") || len(rep.GetNotes()) != 2 ||
		!strings.Contains(rep.GetNotes()[0], "not installed") {
		t.Errorf("hooks on: calls %v, notes %v", r.hooks.calls, rep.GetNotes())
	}
	r.hooks.err = errors.New("settings.json is locked")
	if rep := setSwitch(t, r.s, "hooks", "off", 0); len(rep.GetNotes()) != 2 || !strings.Contains(rep.GetNotes()[0], "locked") {
		t.Errorf("a failed settings change is not reported: %v", rep.GetNotes())
	}
	r.s.enforcement.hooks = nil
	if rep := setSwitch(t, r.s, "hooks", "on", 0); len(rep.GetNotes()) != 1 || !strings.Contains(rep.GetNotes()[0], "orch install") {
		t.Errorf("without orch settings: %v", rep.GetNotes())
	}
}

// TestStatusNamesRelaxedLayers is the orch status half of SP-070.2.a1: while
// a layer is off, Status names it and since when, with or without spaces.
func TestStatusNamesRelaxedLayers(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	if st := r.s.spacesStatus(operatorCtx); len(st.GetRelaxed()) != 0 {
		t.Fatalf("relaxed with nothing switched: %v", st.GetRelaxed())
	}
	setSwitch(t, r.s, "memory", "off", 0)
	r.clock.advance(time.Minute)
	setSwitch(t, r.s, "guard", "log", 0)
	setSwitch(t, r.s, "terms", "off", 0)
	setSwitch(t, r.s, "terms", "on", 0)
	got := r.s.spacesStatus(operatorCtx).GetRelaxed()
	if len(got) != 2 || got[0].GetLayer() != "memory" || got[1].GetLayer() != "guard" ||
		got[0].GetSetAt() != r.clock.now().Add(-time.Minute).Unix() || got[1].GetSetAt() != r.clock.now().Unix() {
		t.Fatalf("relaxed = %v", got)
	}
	if got := r.s.spacesStatus(inClaude).GetRelaxed(); len(got) != 2 || got[0].GetSetBy() != "" {
		t.Errorf("a session's status = %v", got)
	}
	// Without spaces, a relaxed layer still shows; with neither, nothing.
	r.s.spaces = nil
	if st := r.s.spacesStatus(operatorCtx); len(st.GetRelaxed()) != 2 {
		t.Errorf("without spaces: %v", st)
	}
	setSwitch(t, r.s, "memory", "on", 0)
	setSwitch(t, r.s, "guard", "block-other-jobs", 0)
	if st := r.s.spacesStatus(operatorCtx); st != nil {
		t.Errorf("without spaces or relaxed layers: %v", st)
	}
}

// TestEnforcementUnavailable: without a store every layer enforces, and
// switching is refused as unavailable.
func TestEnforcementUnavailable(t *testing.T) {
	s := &server{}
	rep, err := s.GetEnforcement(operatorCtx, &orchdv1.GetEnforcementRequest{})
	if err != nil || rep.GetError() == "" || len(rep.GetSwitches()) != 5 {
		t.Fatalf("GetEnforcement = %v, %v", rep, err)
	}
	for _, sw := range rep.GetSwitches() {
		if sw.GetRelaxed() {
			t.Errorf("%s relaxed without a store", sw.GetLayer())
		}
	}
	if _, err := s.SetEnforcement(operatorCtx, &orchdv1.SetEnforcementRequest{Layer: "guard", State: "off"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("SetEnforcement = %v, want Unavailable", err)
	}
	if newEnforcement(context.Background(), nil, nil) != nil {
		t.Error("switches without a store")
	}
	if st := s.spacesStatus(operatorCtx); st != nil {
		t.Errorf("status = %v", st)
	}
}

// TestEnforcementBrokenStore: orchd's own switches stay in force when
// orch.db cannot be read, which GetEnforcement reports; a switch that cannot
// be written is refused and changes nothing.
func TestEnforcementBrokenStore(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	setSwitch(t, r.s, "guard", "off", 0)
	_ = r.s.enforcement.db.Close()
	rep, err := r.s.GetEnforcement(operatorCtx, &orchdv1.GetEnforcementRequest{})
	if err != nil || !strings.Contains(rep.GetError(), "could not read orch.db") {
		t.Fatalf("GetEnforcement = %v, %v", rep, err)
	}
	if _, err := r.s.SetEnforcement(operatorCtx, &orchdv1.SetEnforcementRequest{Layer: "memory", State: "off"}); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("SetEnforcement = %v, want Internal", err)
	}
	if st := r.s.enforcement.state(spaces.LayerMemory); st != "on" {
		t.Errorf("a switch that could not be written took effect: memory %s", st)
	}
	// A timer still reverts when its revert cannot be written.
	r.s.enforcement.cur[spaces.LayerTerms] = spaces.Switch{Layer: spaces.LayerTerms, State: "off", Until: r.clock.now(), Previous: "on"}
	r.s.enforcement.expire(context.Background())
	if st := r.s.enforcement.state(spaces.LayerTerms); st != "on" {
		t.Errorf("terms after its timer = %s", st)
	}
}

// TestEnforcementStartupAndDrift: orchd starts from orch.db's switches
// when they match their change log, and at its defaults for a layer that
// does not; an edit to orch.db while it runs is reported, never taken.
func TestEnforcementStartupAndDrift(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	setSwitch(t, r.s, "guard", "log", 0)
	conn := r.s.enforcement.db
	again := newEnforcement(context.Background(), conn, nil)
	if sw, problem := again.current(context.Background()); sw.State(spaces.LayerGuard) != "log" || problem != "" {
		t.Fatalf("restart: guard %s, %q", sw.State(spaces.LayerGuard), problem)
	}
	if _, err := conn.Exec(`UPDATE spaces_enforcement SET state = 'off' WHERE layer = 'guard'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO spaces_enforcement (layer, state, set_at, set_by) VALUES ('memory', 'off', '2026-11-11T10:00:00Z', 'operator (process 1)')`); err != nil {
		t.Fatal(err)
	}
	// While orchd runs, its copy wins and the edit shows as drift.
	if st := r.s.enforcement.state(spaces.LayerMemory); st != "on" {
		t.Errorf("a row written outside orchd took effect: memory %s", st)
	}
	rep, _ := r.s.GetEnforcement(inClaude, &orchdv1.GetEnforcementRequest{})
	if rep.GetError() != storeDrift {
		t.Errorf("drift = %q", rep.GetError())
	}
	if st := r.s.spacesStatus(operatorCtx); st.GetEnforcementError() != storeDrift {
		t.Errorf("status = %v", st)
	}
	// At the next start, layers that do not match their log enforce.
	restarted := newEnforcement(context.Background(), conn, nil)
	sw, problem := restarted.current(context.Background())
	if sw.State(spaces.LayerGuard) != "block-other-jobs" || sw.State(spaces.LayerMemory) != "on" ||
		!strings.Contains(problem, "guard switch in orch.db does not match") {
		t.Errorf("restart after the edit: guard %s, memory %s, %q", sw.State(spaces.LayerGuard), sw.State(spaces.LayerMemory), problem)
	}
	// Once orchd writes a layer again, its row and log match.
	r.s.enforcement = restarted
	restarted.now = r.clock.now
	setSwitch(t, r.s, "guard", "strict", 0)
	setSwitch(t, r.s, "memory", "on", 0)
	if _, problem := restarted.current(context.Background()); !strings.Contains(problem, "memory switch") {
		t.Errorf("memory's row written outside orchd still differs: %q", problem)
	}
	if _, err := conn.Exec(`DELETE FROM spaces_enforcement WHERE layer IN ('guard', 'memory')`); err != nil {
		t.Fatal(err)
	}
	if again := newEnforcement(context.Background(), conn, nil); !strings.Contains(again.problem, "guard switch") || again.cur.State(spaces.LayerGuard) != "block-other-jobs" {
		t.Errorf("a deleted row: problem %q, guard %s", again.problem, again.cur.State(spaces.LayerGuard))
	}
	_ = conn.Close()
	if broken := newEnforcement(context.Background(), conn, nil); !strings.Contains(broken.problem, "every layer enforces") || len(broken.cur.Relaxed()) != 0 {
		t.Errorf("unreadable store at startup: %q %v", broken.problem, broken.cur)
	}
}

// TestToolHooksProblem is the drift half of SP-070.1.a4: with the hooks
// switch on, a tool hook turned off or removed in Claude Code's settings
// outside the switch makes the hooks layer not enforcing, which the digest
// and status name; setting the switch on again puts them back.
func TestToolHooksProblem(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	pre, cwd := toolHookIdentities[0], toolHookIdentities[1]
	r.hooks.rows = []settings.Row{
		{Provider: "claude-code", Kind: "hook", Identity: pre, Enabled: true, Present: true},
		{Provider: "claude-code", Kind: "hook", Identity: cwd, Enabled: true, Present: true},
		{Provider: "claude-code", Kind: "hook", Identity: install.HookIdentity("Stop", "stop")},
	}
	if sw := switchesOf(t, r.s, inClaude)["hooks"]; sw.GetRelaxed() || sw.GetProblem() != "" {
		t.Fatalf("installed hooks read as %v", sw)
	}
	r.hooks.rows[1].Present = false
	r.hooks.rows[0].Enabled = false
	if sw := switchesOf(t, r.s, inClaude)["hooks"]; sw.GetRelaxed() {
		t.Errorf("the check was not reused within %s: %v", hooksCheckEvery, sw)
	}
	r.clock.advance(hooksCheckEvery)
	sw := switchesOf(t, r.s, inClaude)["hooks"]
	if !sw.GetRelaxed() || sw.GetState() != "on" || !strings.Contains(sw.GetProblem(), pre+" is disabled, "+cwd+" is missing") {
		t.Fatalf("hooks gone read as %v", sw)
	}
	if got, _ := r.s.relaxedSwitches(operatorCtx); len(got) != 1 || got[0].GetLayer() != "hooks" {
		t.Errorf("status relaxed = %v", got)
	}
	rep := setSwitch(t, r.s, "hooks", "on", 0)
	if rep.GetChanged() || strings.Join(r.hooks.calls, ",") != "claude-code/hook/"+pre+"=on,claude-code/hook/"+cwd+"=on" {
		t.Errorf("hooks on again: changed %v, calls %v", rep.GetChanged(), r.hooks.calls)
	}
	r.hooks.listErr = errors.New("settings unreadable")
	r.clock.advance(hooksCheckEvery)
	if sw := switchesOf(t, r.s, inClaude)["hooks"]; !sw.GetRelaxed() || !strings.Contains(sw.GetProblem(), "could not check") {
		t.Errorf("an unreadable listing reads as %v", sw)
	}
	// A switch already in the state asked changes nothing but the hooks.
	r.hooks.calls = nil
	if rep := setSwitch(t, r.s, "memory", "on", 0); rep.GetChanged() || len(r.hooks.calls) != 0 {
		t.Errorf("memory on again: %v %v", rep, r.hooks.calls)
	}
}

// TestTimerRevertAnnouncesFailedHooks: a timed hooks switch that reverts
// but cannot put the tool hooks back says so in its event.
func TestTimerRevertAnnouncesFailedHooks(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	setSwitch(t, r.s, "hooks", "off", 60)
	r.hooks.err = errors.New("settings.json is locked")
	r.clock.advance(time.Minute)
	r.s.enforcement.expire(context.Background())
	if len(*r.notes) != 2 || len((*r.notes)[1]) != 2 || !strings.Contains((*r.notes)[1][0], "locked") {
		t.Errorf("revert notes = %v", *r.notes)
	}
}

// TestSwitchEventStream: without an emitter a change goes to the spaces
// event stream, which sessions cannot write to as theirs.
func TestSwitchEventStream(t *testing.T) {
	s := spacesServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.enforcement = newEnforcement(context.Background(), conn, nil)
	setSwitch(t, s, "terms", "off", 0)
	evs, err := events.Read(paths.EventsDir(), spacesEventStream)
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Summary, "terms (outbound terms) switched from on to off by operator") {
		t.Fatalf("events = %v, %v", evs, err)
	}
	// The operator's stream is owner-only from its first write (SP-048).
	if fi, err := os.Stat(filepath.Join(paths.EventsDir(), spacesEventStream+".jsonl")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stream mode: %v, %v", fi, err)
	}
}

// TestToolEventsNotEnforced is SP-070.2.a2: with the guard off, tool events
// are still logged, with decision "not enforced" and the verdict the guard
// would have given; logging only records the verdict; enforcing states
// leave the record to the guard.
func TestToolEventsNotEnforced(t *testing.T) {
	s, dirs := pinsServer(t)
	withToolLog(t, s)
	r := withEnforcement(t, s)
	var asked []string
	s.guardVerdict = func(rec toollog.Record) (string, string) {
		asked = append(asked, rec.Command)
		if strings.Contains(rec.Command, "acme") {
			return "refuse", "other-job"
		}
		return "allow", ""
	}
	if _, err := s.PinSession(inClaude, pinReq("sess-g", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	ctx := pinnedCtx(t, s, "sess-g")
	send := func(cmd string) {
		t.Helper()
		if _, err := s.RecordToolEvent(ctx, bashReq("sess-g", dirs[0], cmd)); err != nil {
			t.Fatal(err)
		}
		flushToolLog(t, s)
	}
	send("ls ~/code/acme")
	setSwitch(t, r.s, "guard", "off", 0)
	send("ls ~/code/acme")
	send("ls")
	if _, err := s.RecordToolEvent(ctx, &orchdv1.RecordToolEventRequest{Event: "cwd-changed", SessionId: "sess-g", OldCwd: dirs[0], NewCwd: dirs[1]}); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, s)
	setSwitch(t, r.s, "guard", "log", 0)
	send("cat acme.txt")
	setSwitch(t, r.s, "guard", "strict", 0)
	send("ls")

	recs := records(t, s, sessionOwner("sess-g"))
	want := []struct{ decision, rule string }{
		{decisionLogged, ""},
		{decisionNotEnforced, "would refuse (other-job)"},
		{decisionNotEnforced, "would allow"},
		{decisionNotEnforced, "would allow"},
		{decisionLogged, "would refuse (other-job)"},
		{decisionLogged, ""},
	}
	if len(recs) != len(want) {
		t.Fatalf("%d records, want %d: logging must never stop", len(recs), len(want))
	}
	for i, w := range want {
		if recs[i].Decision != w.decision || recs[i].Rule != w.rule {
			t.Errorf("record %d (%s %s) = %q %q, want %q %q", i, recs[i].Event, recs[i].Command, recs[i].Decision, recs[i].Rule, w.decision, w.rule)
		}
	}
	if len(asked) != 4 {
		t.Errorf("the guard was asked %d times, want 4 (off and log only)", len(asked))
	}
	// Without a guard verdict yet, an off guard still marks its events.
	s.guardVerdict = nil
	setSwitch(t, r.s, "guard", "off", 0)
	send("ls")
	if last := records(t, s, sessionOwner("sess-g")); last[len(last)-1].Decision != decisionNotEnforced || last[len(last)-1].Rule != "" {
		t.Errorf("last record = %+v", last[len(last)-1])
	}
	if v, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-g"}); err != nil || len(v.GetProblems()) != 0 {
		t.Errorf("chain after not-enforced records: %v, %v", v, err)
	}
}

// TestToolHooksSwitchRealSettings runs the hooks switch against the real
// settings service over a temporary settings.json: off removes the two
// tool hooks and leaves the others, on puts them back, and a tool hook
// deleted from the file by hand makes the layer not enforcing.
func TestToolHooksSwitchRealSettings(t *testing.T) {
	s, path := settingsServer(t)
	hooks := `{"hooks":{` +
		`"PreToolUse":[{"hooks":[{"type":"command","command":"/x/orch-hook pre-tool-use"}]}],` +
		`"CwdChanged":[{"hooks":[{"type":"command","command":"/x/orch-hook cwd-changed"}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"/x/orch-hook stop"}]}]}}`
	if err := os.WriteFile(path, []byte(hooks), 0o644); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.enforcement = newEnforcement(context.Background(), conn, s.settings)
	s.enforcement.emit = func(spaces.SwitchChange, []string) {}
	read := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if sw := switchesOf(t, s, inClaude)["hooks"]; sw.GetRelaxed() {
		t.Fatalf("installed hooks read as %v", sw)
	}
	if rep := setSwitch(t, s, "hooks", "off", 0); len(rep.GetNotes()) != 0 {
		t.Fatalf("hooks off: %v", rep.GetNotes())
	}
	if f := read(); strings.Contains(f, "pre-tool-use") || strings.Contains(f, "cwd-changed") || !strings.Contains(f, "orch-hook stop") {
		t.Errorf("settings.json after hooks off:\n%s", f)
	}
	setSwitch(t, s, "hooks", "on", 0)
	if f := read(); !strings.Contains(f, "orch-hook pre-tool-use") || !strings.Contains(f, "orch-hook cwd-changed") {
		t.Errorf("settings.json after hooks on:\n%s", f)
	}
	// Deleted by hand while the switch is on.
	if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/orch-hook stop"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.enforcement.forgetHooksCheck()
	sw := switchesOf(t, s, inClaude)["hooks"]
	if !sw.GetRelaxed() || !strings.Contains(sw.GetProblem(), "orch-hook pre-tool-use is missing") {
		t.Fatalf("hooks deleted by hand read as %v", sw)
	}
	setSwitch(t, s, "hooks", "on", 0)
	if f := read(); !strings.Contains(f, "orch-hook pre-tool-use") {
		t.Errorf("hooks on did not put the deleted hook back:\n%s", f)
	}
	if sw := switchesOf(t, s, inClaude)["hooks"]; sw.GetRelaxed() {
		t.Errorf("after putting them back: %v", sw)
	}

	// Rows deleted from orch.db along with the hooks in the file: with no
	// spaces applied nothing is required; with a policy applied, orch's
	// other hooks being installed, they count as not installed.
	if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/orch-hook stop"}]}],`+
		`"SessionStart":[{"hooks":[{"type":"command","command":"/x/orch-hook session-start"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.enforcement.forgetHooksCheck()
	_ = switchesOf(t, s, inClaude)
	// The stored origin is not trusted either.
	if _, err := conn.Exec(`DELETE FROM settings_items WHERE identity IN (?, ?)`, toolHookIdentities[0], toolHookIdentities[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE settings_items SET origin = 'found' WHERE provider = 'claude-code'`); err != nil {
		t.Fatal(err)
	}
	s.enforcement.forgetHooksCheck()
	if sw := switchesOf(t, s, inClaude)["hooks"]; sw.GetRelaxed() {
		t.Errorf("without spaces, deleted rows read as %v", sw)
	}
	sp, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }, LookupOrg: orgNodes{}.lookup})
	if err != nil {
		t.Fatal(err)
	}
	s.spaces = sp
	d := draft(t, s, `{"spaces":[{"name":"personal","kind":"private"}]}`)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	sw = switchesOf(t, s, inClaude)["hooks"]
	if !sw.GetRelaxed() || !strings.Contains(sw.GetProblem(), "PreToolUse/orch-hook pre-tool-use is not installed, CwdChanged/orch-hook cwd-changed is not installed") {
		t.Errorf("with spaces, deleted rows read as %v", sw)
	}
}

// TestHooksCheckIgnoresCallerDeadline: a caller whose deadline has passed
// does not decide the shared check.
func TestHooksCheckIgnoresCallerDeadline(t *testing.T) {
	r := withEnforcement(t, spacesServer(t))
	r.hooks.rows = []settings.Row{{Provider: "claude-code", Kind: "hook", Identity: toolHookIdentities[0], Enabled: true, Present: true}}
	r.hooks.providers = map[string]settings.ProviderState{"claude-code": {Error: "parse settings.json: unexpected end"}}
	if p := r.s.enforcement.hooksProblem(context.Background(), false); !strings.Contains(p, "could not check Claude Code's tool hooks: parse") {
		t.Errorf("a failed sync reads as %q", p)
	}
	r.hooks.providers = nil
	r.s.enforcement.forgetHooksCheck()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p := r.s.enforcement.hooksProblem(ctx, false); p != "" {
		t.Errorf("a cancelled caller's check = %q", p)
	}
	if r.s.spacesConfigured() {
		t.Error("an empty store reads as configured")
	}
	if (&server{}).spacesConfigured() {
		t.Error("no spaces reads as configured")
	}
}
