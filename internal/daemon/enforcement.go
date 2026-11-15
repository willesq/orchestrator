package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// The enforcement switches (spaces SP-070). See docs/internals/daemon.md
// "Enforcement switches".

// spacesEventStream is where orchd reports switch changes, for the app and
// the operator; orch-hook reports refusals there too (SP-048). Sessions
// learn of a switched-off layer from GetEnforcement, never from this
// stream, which any process running as the user can append to.
const spacesEventStream = events.SpacesStream

// enforcementTick is how often orchd reverts timed switches whose time is
// up. Every read also reverts them first, so a reader never sees an expired
// switch; the tick only makes the revert's event arrive on time.
const enforcementTick = 5 * time.Second

// storeDrift is reported while orch.db's switches differ from orchd's.
const storeDrift = "the enforcement switches in orch.db were changed outside orchd; orchd keeps the ones it set"

// toolHookIdentities are the Claude Code settings items of the tool hooks,
// which only the hooks switch turns on and off (D-40).
var toolHookIdentities = []string{
	install.HookIdentity("PreToolUse", "pre-tool-use"),
	install.HookIdentity("CwdChanged", "cwd-changed"),
}

// sessionStartHook is the settings item of orch's session-start hook, which
// prints the digest.
var sessionStartHook = install.HookIdentity("SessionStart", "session-start")

// isToolHook reports whether a settings item is one of the tool hooks.
func isToolHook(provider, kind, identity string) bool {
	if provider != install.ProviderClaudeCode || kind != install.KindHook {
		return false
	}
	for _, id := range toolHookIdentities {
		if id == identity {
			return true
		}
	}
	return false
}

// hookSettings turns Claude Code's settings items on and off and lists
// them; the settings service in orchd, a fake in tests.
type hookSettings interface {
	SetEnabled(ctx context.Context, provider, kind, identity string, enabled bool) (settings.Row, error)
	List(ctx context.Context, provider, kind string) (settings.Listing, error)
}

// enforcement holds the switches. orchd's copy in memory is the one
// enforced while it runs; each change is written to orch.db first, and a
// difference found there later is reported as drift, never taken.
type enforcement struct {
	db  *sql.DB
	now func() time.Time
	// emit reports a change; nil appends it to the spaces event stream.
	emit func(spaces.SwitchChange, []string)
	// hooks applies the hooks switch to Claude Code's settings; nil leaves
	// the files alone.
	hooks hookSettings
	// onChange, when set, is given every switch after a change, under mu:
	// orchd rewrites the guard's snapshot with it (SP-041).
	onChange func(spaces.Enforcement)
	// onSandbox asks for a sync of the per-checkout settings (SP-046) when
	// the sandbox switch changes; nil leaves them to the next periodic sync.
	onSandbox func()

	// mu guards cur and problem, and orders changes, so a switch and its
	// side effects are not interleaved with another's.
	mu  sync.Mutex
	cur spaces.Enforcement
	// problem says why orch.db's switches were not all taken at startup, or
	// differ from cur now; "" when they match.
	problem string
	// sandbox is the sandbox setting (D-61), and sandboxProblem why orch.db's
	// was not taken at startup or differs from it now.
	sandbox        spaces.SandboxSetting
	sandboxProblem string

	// hooksMu guards the cached hooks check: every caller may ask, and a
	// check reads Claude Code's settings.
	hooksMu       sync.Mutex
	hooksAt       time.Time
	hooksRequired bool
	hooksLast     string
}

// hooksCheckEvery is how long a check of Claude Code's tool hooks is
// reused.
const hooksCheckEvery = 5 * time.Second

// newEnforcement serves the switches in db, applying the hooks switch
// through svc; nil when there is no store. A layer whose row does not match
// its change log starts at its default.
func newEnforcement(ctx context.Context, db *sql.DB, svc *settings.Service) *enforcement {
	if db == nil {
		return nil
	}
	e := &enforcement{db: db, now: time.Now}
	// Reason: a nil *settings.Service in the interface would not compare
	// equal to nil.
	if svc != nil {
		e.hooks = svc
	}
	cur, problems, err := spaces.LoadVerified(ctx, db)
	switch {
	case err != nil:
		cur, e.problem = spaces.DefaultEnforcement(), "could not read the enforcement switches; every layer enforces: "+err.Error()
	case len(problems) > 0:
		e.problem = strings.Join(problems, "; ") + "; those layers enforce"
	}
	for _, p := range problems {
		log.Printf("orchd: enforcement: %s", p)
	}
	e.cur = cur
	e.loadSandbox(ctx)
	return e
}

// current returns every layer's switch, reverting expired timed switches
// first, and what is wrong with orch.db's copy, if anything. Without a
// store every layer is at its default, which enforces.
func (e *enforcement) current(ctx context.Context) (spaces.Enforcement, string) {
	if e == nil {
		return spaces.DefaultEnforcement(), "enforcement switches are unavailable; every layer enforces"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked(ctx)
	stored, err := spaces.LoadEnforcement(ctx, e.db)
	switch {
	case err != nil:
		return e.copyLocked(), "could not read orch.db's enforcement switches; orchd keeps the ones it set: " + err.Error()
	case spaces.SameSwitches(stored, e.cur):
		// orch.db holds what orchd enforces again (it rewrote the layers).
		e.problem = ""
	case e.problem == "":
		log.Printf("orchd: enforcement: %s", storeDrift)
		e.problem = storeDrift
	}
	return e.copyLocked(), e.problem
}

func (e *enforcement) copyLocked() spaces.Enforcement {
	out := make(spaces.Enforcement, len(e.cur))
	for l, s := range e.cur {
		out[l] = s
	}
	return out
}

// state is layer l's state. It only reads orchd's copy: a timed switch
// whose time is up reads as reverted, and the revert itself is left to
// current and the tick. Reason: the command log's writer calls it for every
// event, so it must not wait on orch.db or reach anything that writes
// elsewhere (settings files, RAG).
func (e *enforcement) state(l spaces.Layer) string {
	if e == nil {
		return l.Default()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cur.At(e.now()).State(l)
}

// set switches layer l and applies its side effects. notes says what the
// side effects did, for the operator. Setting the hooks layer to the state
// it has applies it to Claude Code's settings again.
func (e *enforcement) set(ctx context.Context, l spaces.Layer, state, by string, forDur time.Duration) (bool, []string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked(ctx)
	c, sw, changed, err := spaces.NextSwitch(e.cur, l, state, by, forDur, e.now())
	if err != nil {
		return false, nil, err
	}
	if !changed {
		if l == spaces.LayerHooks {
			return false, e.applyHooks(ctx, state == spaces.SwitchOn), nil
		}
		return false, nil, nil
	}
	if err := spaces.WriteSwitch(ctx, e.db, c, sw); err != nil {
		return false, nil, err
	}
	e.cur[l] = sw
	return true, e.changed(ctx, c), nil
}

// expire reverts every timed switch whose time is up.
func (e *enforcement) expire(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked(ctx)
}

// expireLocked reverts expired timed switches. A revert that cannot be
// written to orch.db still takes effect: the timer is what the operator
// chose, and the difference then shows as drift.
func (e *enforcement) expireLocked(ctx context.Context) {
	cs, sws := spaces.Expired(e.cur, e.now())
	for i, c := range cs {
		if err := spaces.WriteSwitch(ctx, e.db, c, sws[i]); err != nil {
			log.Printf("orchd: enforcement: record the revert of %s: %v", c.Layer, err)
		}
		e.cur[c.Layer] = sws[i]
		e.changed(ctx, c)
	}
}

// changed logs and announces c, applies its side effects and returns what
// they did. A side effect that failed is part of the announcement, so a
// timer's revert that could not put the tool hooks back is not silent.
func (e *enforcement) changed(ctx context.Context, c spaces.SwitchChange) []string {
	var notes []string
	if c.Layer == spaces.LayerHooks {
		notes = e.applyHooks(ctx, c.State == spaces.SwitchOn)
	}
	if e.onChange != nil {
		e.onChange(e.copyLocked())
	}
	if c.Layer == spaces.LayerSandbox && e.onSandbox != nil {
		e.onSandbox()
	}
	log.Printf("orchd: enforcement: %s", c.Describe())
	for _, n := range notes {
		log.Printf("orchd: enforcement: %s", n)
	}
	if e.emit != nil {
		e.emit(c, notes)
		return notes
	}
	summary := c.Describe()
	if len(notes) > 0 {
		summary += "; " + strings.Join(notes, "; ")
	}
	if err := events.AppendSpaces(paths.EventsDir(), events.Event{
		Kind: "info", Source: "enforcement", Summary: summary, Timestamp: c.At.UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("orchd: enforcement: record event: %v", err)
	}
	return notes
}

// applyHooks turns Claude Code's tool hooks on or off in its settings, the
// one place the hooks switch acts outside orchd. Codex's hooks file is not
// managed by orch settings, so its pre-tool-use hook stays (see
// docs/internals/hooks.md).
func (e *enforcement) applyHooks(ctx context.Context, on bool) []string {
	if e.hooks == nil {
		return []string{"orch settings are unavailable, so Claude Code's settings.json was not changed; run 'orch install' to apply the switch"}
	}
	defer e.forgetHooksCheck()
	var notes []string
	for _, id := range toolHookIdentities {
		_, err := e.hooks.SetEnabled(ctx, install.ProviderClaudeCode, install.KindHook, id, on)
		switch {
		case errors.Is(err, settings.ErrNotFound):
			notes = append(notes, fmt.Sprintf("Claude Code hook %s is not installed; nothing to change", id))
		case err != nil:
			notes = append(notes, fmt.Sprintf("could not change Claude Code hook %s: %v", id, err))
		}
	}
	return notes
}

// hooksProblem says why the tool hooks do not run though the hooks switch
// is on: a tool hook orch installed in Claude Code's settings that is now
// turned off, missing or removed from the file, or a settings listing it
// cannot read. With required set (a spaces policy is applied) and orch's
// session-start hook in Claude Code's settings, a tool hook with no row at
// all counts too: deleting its row cannot hide it, since the sync recreates
// the session-start hook's row from the file the digest runs from. "" when
// they run.
func (e *enforcement) hooksProblem(ctx context.Context, required bool) string {
	if e == nil || e.hooks == nil {
		return ""
	}
	e.hooksMu.Lock()
	defer e.hooksMu.Unlock()
	now := e.now()
	if !e.hooksAt.IsZero() && e.hooksRequired == required && now.Before(e.hooksAt.Add(hooksCheckEvery)) {
		return e.hooksLast
	}
	// Reason: the result is shared by every caller for a few seconds, so a
	// caller's short deadline must not decide it.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	l, err := e.hooks.List(lctx, install.ProviderClaudeCode, install.KindHook)
	e.hooksAt, e.hooksRequired = now, required
	if err != nil {
		e.hooksLast = "could not check Claude Code's tool hooks: " + err.Error()
		return e.hooksLast
	}
	if st := l.Providers[install.ProviderClaudeCode]; st.Error != "" {
		e.hooksLast = "could not check Claude Code's tool hooks: " + st.Error
		return e.hooksLast
	}
	var off []string
	seen, digestHook := map[string]bool{}, false
	for _, r := range l.Rows {
		// Reason: judged by identity, which the sync derives from the file
		// on every look, never by the stored origin, which it keeps.
		if r.Provider == install.ProviderClaudeCode && r.Kind == install.KindHook && r.Identity == sessionStartHook && r.Present {
			digestHook = true
		}
		if !isToolHook(r.Provider, r.Kind, r.Identity) {
			continue
		}
		seen[r.Identity] = true
		// A row exists once orch saw the hook in the file, so "missing"
		// (orch's hook gone from settings.json) is a hook that was removed.
		if r.Status() != settings.StatusActive {
			off = append(off, r.Identity+" is "+string(r.Status()))
		}
	}
	if required && digestHook {
		for _, id := range toolHookIdentities {
			if !seen[id] {
				off = append(off, id+" is not installed")
			}
		}
	}
	e.hooksLast = ""
	if len(off) > 0 {
		e.hooksLast = "the switch is on but Claude Code's " + strings.Join(off, ", ") +
			"; put them back with 'orch spaces enforce hooks on'"
	}
	return e.hooksLast
}

// forgetHooksCheck makes the next hooksProblem check again, after the
// switch changed the hooks.
func (e *enforcement) forgetHooksCheck() {
	e.hooksMu.Lock()
	defer e.hooksMu.Unlock()
	e.hooksAt = time.Time{}
}

// run reverts timed switches on time until ctx ends.
func (e *enforcement) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.expire(ctx)
		}
	}
}

// switches returns every layer for the wire, with the hooks layer marked
// as not enforcing when its hooks are gone though its switch is on.
func (s *server) switches(ctx context.Context) ([]*orchdv1.EnforcementSwitch, string) {
	sw, problem := s.enforcement.current(ctx)
	_, isOp := operator(ctx)
	out := make([]*orchdv1.EnforcementSwitch, 0, len(spaces.Layers))
	for _, l := range spaces.Layers {
		p := switchProto(sw[l], isOp)
		if l == spaces.LayerHooks && sw.State(l) == spaces.SwitchOn {
			if hp := s.enforcement.hooksProblem(ctx, s.spacesConfigured()); hp != "" {
				p.Relaxed, p.Problem = true, hp
			}
		}
		out = append(out, p)
	}
	return out, problem
}

func (s *server) GetEnforcement(ctx context.Context, _ *orchdv1.GetEnforcementRequest) (*orchdv1.EnforcementState, error) {
	sw, problem := s.switches(ctx)
	return &orchdv1.EnforcementState{Switches: sw, Error: problem, SandboxSetting: s.sandboxProto(ctx)}, nil
}

func (s *server) SetEnforcement(ctx context.Context, req *orchdv1.SetEnforcementRequest) (*orchdv1.SetEnforcementResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SetEnforcement_FullMethodName, "switch enforcement"); err != nil {
		return nil, err
	}
	if s.enforcement == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "enforcement switches are unavailable")
	}
	l, err := spaces.ParseLayer(req.GetLayer())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	secs := req.GetForSeconds()
	if secs < 0 || secs > int64(spaces.MaxSwitchFor/time.Second) {
		return nil, grpcstatus.Error(codes.InvalidArgument, spaces.ErrBadDuration.Error())
	}
	changed, notes, err := s.enforcement.set(ctx, l, req.GetState(), callerLabel(ctx), time.Duration(secs)*time.Second)
	switch {
	case errors.Is(err, spaces.ErrBadState), errors.Is(err, spaces.ErrBadDuration):
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	sws, _ := s.switches(ctx)
	var cur *orchdv1.EnforcementSwitch
	for _, p := range sws {
		if p.GetLayer() == string(l) {
			cur = p
		}
	}
	return &orchdv1.SetEnforcementResponse{Current: cur, Changed: changed, Notes: notes}, nil
}

// switchProto is sw for the wire; who set it only for the operator.
func switchProto(sw spaces.Switch, operator bool) *orchdv1.EnforcementSwitch {
	out := &orchdv1.EnforcementSwitch{
		Layer: string(sw.Layer), Title: sw.Layer.Title(), State: sw.State, DefaultState: sw.Layer.Default(),
		States: sw.Layer.States(), Relaxed: sw.Relaxed(), SetAt: unixOrZero(sw.SetAt), Until: unixOrZero(sw.Until),
	}
	if !sw.Until.IsZero() {
		out.RevertsTo = sw.Previous
	}
	if operator {
		out.SetBy = sw.SetBy
	}
	return out
}

// relaxedSwitches lists the switches that stop their layer enforcing, for
// orch status, and what is wrong with orch.db's copy.
func (s *server) relaxedSwitches(ctx context.Context) ([]*orchdv1.EnforcementSwitch, string) {
	sws, problem := s.switches(ctx)
	var out []*orchdv1.EnforcementSwitch
	for _, p := range sws {
		if p.GetRelaxed() {
			out = append(out, p)
		}
	}
	if s.enforcement == nil {
		problem = ""
	}
	return out, problem
}

// refuseToolHookSetting refuses turning a tool hook on or off through orch
// settings: only the operator's hooks switch does (D-40).
func refuseToolHookSetting(provider, kind, identity string) error {
	if !isToolHook(provider, kind, identity) {
		return nil
	}
	return grpcstatus.Error(codes.FailedPrecondition,
		"the tool hooks (pre-tool-use, cwd-changed) are an enforcement switch; the operator turns them off with 'orch spaces enforce hooks off'")
}

// decisionNotEnforced is the decision of a tool event taken while the guard
// is switched off: logged, never decided (SP-070.2).
const decisionNotEnforced = "not enforced"

// guardVerdictFunc is the verdict the guard would give a call, "allow" or
// "refuse", and its rule id.
type guardVerdictFunc func(r toollog.Record) (verdict, rule string)

// markGuardState records the guard's switch on a tool event no guard
// decided (decision logged). With the guard off the decision is "not
// enforced"; off or logging, the rule holds the verdict the guard would
// have given, when a guard verdict is available (s.guardVerdict, wired by
// SP-040/SP-041). Enforcing states, and any decision a guard made, are
// left as they are. The state is the one when orchd stores the event, a
// few milliseconds after the hook ran.
func (s *server) markGuardState(r *toollog.Record) {
	if r.Decision != decisionLogged {
		return
	}
	switch s.enforcement.state(spaces.LayerGuard) {
	case spaces.SwitchOff:
		r.Decision = decisionNotEnforced
	case spaces.SwitchLog:
	default:
		return
	}
	if s.guardVerdict == nil {
		return
	}
	if v, rule := s.guardVerdict(*r); v != "" {
		r.Rule = toollog.Clean(wouldRule(v, rule), toollog.MaxField)
	}
}

// wouldRule is the stored rule of a call the guard did not enforce.
func wouldRule(verdict, rule string) string {
	if rule == "" {
		return "would " + verdict
	}
	return "would " + verdict + " (" + rule + ")"
}

// spacesConfigured reports whether a spaces policy with spaces is applied.
func (s *server) spacesConfigured() bool {
	if s.spaces == nil {
		return false
	}
	r, err := s.spaces.Current()
	return err != nil || (r != nil && r.Configured())
}
