package daemon

import (
	"context"
	"errors"
	"log"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The sandbox setting (spaces D-61): auto | on | off, the operator's lasting
// choice of whether orch writes its sandbox rules. orchd holds it beside the
// switches, under the same lock, and like them writes each change to
// orch.db first. Unlike a switch it never marks a start digest (INV-16 as
// amended); orch status and the app show it. See docs/internals/daemon.md
// "Sandbox setting".

// sandboxDrift is reported while orch.db's setting differs from orchd's.
const sandboxDrift = "the sandbox setting in orch.db was changed outside orchd; orchd keeps the one it set"

// loadSandbox takes the setting from orch.db at startup; one that does not
// match its change log, or cannot be read, is on, which writes the rules.
func (e *enforcement) loadSandbox(ctx context.Context) {
	s, problem, err := spaces.LoadSandboxSetting(ctx, e.db)
	if err != nil {
		s, problem = spaces.SandboxSetting{Mode: spaces.SandboxOn}, "could not read the sandbox setting; orch writes the sandbox rules (on): "+err.Error()
	}
	if problem != "" {
		log.Printf("orchd: enforcement: %s", problem)
	}
	e.sandbox, e.sandboxProblem = s, problem
}

// sandboxSetting returns the setting orchd applies and what is wrong with
// orch.db's copy, if anything. Without a store it is auto.
func (e *enforcement) sandboxSetting(ctx context.Context) (spaces.SandboxSetting, string) {
	if e == nil {
		return spaces.DefaultSandboxSetting(), ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, problem, err := spaces.LoadSandboxSetting(ctx, e.db)
	switch {
	case err != nil:
		return e.sandbox, "could not read orch.db's sandbox setting; orchd keeps the one it set: " + err.Error()
	case problem == "" && sameSandbox(stored, e.sandbox):
		e.sandboxProblem = ""
	case e.sandboxProblem == "":
		log.Printf("orchd: enforcement: %s", sandboxDrift)
		e.sandboxProblem = sandboxDrift
	}
	return e.sandbox, e.sandboxProblem
}

func sameSandbox(a, b spaces.SandboxSetting) bool {
	return a.Mode == b.Mode && a.SetBy == b.SetBy && a.SetAt.Equal(b.SetAt)
}

// choice is the sandbox layer's state for a sync or a launch: the switch
// (a timed switch whose time is up reads as reverted) and the setting.
func (e *enforcement) choice() spacesettings.Choice {
	if e == nil {
		return spacesettings.ChoiceAuto
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return spacesettings.Choice{
		SwitchedOff: e.cur.At(e.now()).State(spaces.LayerSandbox) == spaces.SwitchOff,
		Setting:     e.sandbox.Mode,
	}
}

// setSandbox sets the setting as by, logs and announces the change and asks
// for a sync of the checkouts. changed is false when it had that mode.
func (e *enforcement) setSandbox(ctx context.Context, mode spaces.SandboxMode, by string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, next, changed, err := spaces.NextSandboxSetting(e.sandbox, mode, by, e.now())
	if err != nil || !changed {
		return false, err
	}
	if err := spaces.WriteSandboxSetting(ctx, e.db, c, next); err != nil {
		return false, err
	}
	// orch.db now holds what orchd applies.
	e.sandbox, e.sandboxProblem = next, ""
	if e.onSandbox != nil {
		e.onSandbox()
	}
	summary := spaces.DescribeSandboxChange(c)
	log.Printf("orchd: enforcement: %s", summary)
	if e.emit != nil {
		e.emit(c, nil)
		return true, nil
	}
	if err := events.AppendSpaces(paths.EventsDir(), events.Event{
		Kind: "info", Source: "enforcement", Summary: summary, Timestamp: c.At.UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("orchd: enforcement: record event: %v", err)
	}
	return true, nil
}

func (s *server) SetSandboxSetting(ctx context.Context, req *orchdv1.SetSandboxSettingRequest) (*orchdv1.SetSandboxSettingResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SetSandboxSetting_FullMethodName, "change the sandbox setting"); err != nil {
		return nil, err
	}
	if s.enforcement == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "the sandbox setting is unavailable")
	}
	mode, err := spaces.ParseSandboxMode(req.GetMode())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	changed, err := s.enforcement.setSandbox(ctx, mode, callerLabel(ctx))
	switch {
	case errors.Is(err, spaces.ErrBadSandboxMode):
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	return &orchdv1.SetSandboxSettingResponse{Current: s.sandboxProto(ctx), Changed: changed}, nil
}

// sandboxProto is the setting for the wire, with whether some space's
// sessions get orch's sandbox rules now; who set it only for the operator.
func (s *server) sandboxProto(ctx context.Context) *orchdv1.SandboxSetting {
	set, problem := s.enforcement.sandboxSetting(ctx)
	out := &orchdv1.SandboxSetting{
		Mode: string(set.Mode), DefaultMode: string(spaces.SandboxAuto), SetAt: unixOrZero(set.SetAt), Problem: problem,
	}
	for _, m := range spaces.SandboxModes {
		out.Modes = append(out.Modes, string(m))
	}
	if _, ok := operator(ctx); ok {
		out.SetBy = set.SetBy
	}
	var res *spaces.Resolved
	if s.spaces != nil {
		res, _ = s.spaces.Current()
	}
	l := spacesettings.DefaultLayout()
	if s.checkouts != nil {
		l = s.checkouts.syncer.Layout
	}
	ch := s.enforcement.choice()
	out.Active = ch.AnySandboxed(res, l)
	out.Detail = sandboxDetail(ch, res.Configured(), out.Active)
	return out
}

// sandboxDetail says in words what the setting does now.
func sandboxDetail(ch spacesettings.Choice, configured, active bool) string {
	switch {
	case ch.SwitchedOff:
		return "the sandbox switch is off ('orch spaces enforce'), so no sandbox rules are written"
	case !configured:
		return "no spaces policy is applied, so there is nothing to sandbox"
	case ch.Setting == spaces.SandboxOff:
		return "off by choice: orch writes no sandbox rules; the tool guard, memory filtering and outbound terms still apply"
	case ch.Setting == spaces.SandboxOn:
		return "on: every space's sessions get orch's sandbox rules"
	case active:
		return "auto: sandbox rules for the sessions another isolated space's folders are denied to"
	}
	return "auto: no other isolated space's folders to deny, so no sandbox rules are written"
}
