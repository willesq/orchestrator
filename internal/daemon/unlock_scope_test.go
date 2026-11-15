package daemon

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// interceptorLets reports whether orchd's interceptors let method with req
// from process pid over conn reach its handler, and their refusal if not.
func (e *unlockEnv) interceptorLets(pid int, conn net.Conn, method string, req any) (bool, error) {
	ctx := callerCtx(e.authOn(pid, conn))
	info := &grpc.UnaryServerInfo{FullMethod: method}
	ran := false
	_, err := guardQueuedCallers(ctx, req, info, func(ctx context.Context, req any) (any, error) {
		return e.r.unary(ctx, req, info, func(context.Context, any) (any, error) {
			ran = true
			return nil, nil
		})
	})
	return ran, err
}

// resolvedCtx is a call's context from process pid as the resolver sees
// it, for handlers the interceptors would answer first.
func (e *unlockEnv) resolvedCtx(pid int) context.Context {
	ctx := callerCtx(e.auth(pid))
	return context.WithValue(ctx, resolvedKey{}, e.r.resolve(ctx))
}

// weakeningRPCs are the RPCs D-78 keeps Orch.app's alone under an unlock,
// by group, with a request each.
func weakeningRPCs(t *testing.T) map[string]map[string]any {
	return map[string]map[string]any{
		"policy":            {orchdv1.Orchd_ApplySpaces_FullMethodName: &orchdv1.ApplySpacesRequest{ReviewedRevision: 1}},
		"switches":          {orchdv1.Orchd_SetEnforcement_FullMethodName: &orchdv1.SetEnforcementRequest{Layer: "memory", State: " OFF"}},
		"guard log":         {orchdv1.Orchd_SetEnforcement_FullMethodName: &orchdv1.SetEnforcementRequest{Layer: "guard", State: "log"}},
		"unknown layer":     {orchdv1.Orchd_SetEnforcement_FullMethodName: &orchdv1.SetEnforcementRequest{Layer: "nope", State: "on"}},
		"sandbox":           {orchdv1.Orchd_SetSandboxSetting_FullMethodName: &orchdv1.SetSandboxSettingRequest{}},
		"outbound":          {orchdv1.Orchd_ChangeOutboundRule_FullMethodName: &orchdv1.ChangeOutboundRuleRequest{Action: "disable", Id: "x"}},
		"terms (SP-053)":    {orchdv1.Orchd_SetSpaceTerms_FullMethodName: &orchdv1.SetSpaceTermsRequest{Space: "x"}},
		"settings (SP-078)": settingsRPCs(t),
	}
}

// TestUnlockCannotWeakenProtection is D-78 (RC-1150): a terminal under a
// confirmed unlock is refused every RPC that weakens protection (applying
// a policy, switching enforcement off, the sandbox setting, outbound rules
// and SP-078's settings RPCs) before the handler runs, while Orch.app is let
// through; the unlocked terminal keeps reads, relabel, retag and tightening
// a switch.
func TestUnlockCannotWeakenProtection(t *testing.T) {
	e := newUnlockEnv(t)
	e.grant(t, 32, 600)
	for group, rpcs := range weakeningRPCs(t) {
		for method, req := range rpcs {
			t.Run(group+"/"+rpcName(method), func(t *testing.T) {
				ran, err := e.interceptorLets(34, unsigned, method, req)
				if ran || grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "D-78") {
					t.Fatalf("from the unlocked terminal: ran %v, %v; want refused", ran, err)
				}
				if ran, err := e.interceptorLets(50, genuineApp, method, req); !ran || err != nil {
					t.Fatalf("from Orch.app: ran %v, %v; want let through", ran, err)
				}
			})
		}
	}
	// Through the interceptors and the handler, nothing is switched.
	if _, err := e.call(34, unsigned, orchdv1.Orchd_SetEnforcement_FullMethodName, func(ctx context.Context) (any, error) {
		return e.s.SetEnforcement(ctx, &orchdv1.SetEnforcementRequest{Layer: "memory", State: spaces.SwitchOff})
	}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("SetEnforcement off through the interceptors = %v", err)
	}
	if sw := switchesOf(t, e.s, operatorCtx)["memory"]; sw.GetState() == spaces.SwitchOff {
		t.Fatal("the unlocked terminal switched memory filtering off")
	}

	// Allowed: reads of every space, relabel, retag, tightening a switch,
	// and SetConfig without a Sensitive key.
	if !e.operatorScope(t, 34) {
		t.Fatal("the unlocked terminal does not read every space")
	}
	for name, c := range map[string]struct {
		method string
		req    any
	}{
		"relabel":         {orchdv1.Orchd_Relabel_FullMethodName, &orchdv1.RelabelRequest{}},
		"retag":           {orchdv1.Orchd_RetagSource_FullMethodName, &orchdv1.RetagSourceRequest{}},
		"diff":            {orchdv1.Orchd_DiffSpaces_FullMethodName, &orchdv1.DiffSpacesRequest{}},
		"guard tightened": {orchdv1.Orchd_SetEnforcement_FullMethodName, &orchdv1.SetEnforcementRequest{Layer: "guard", State: "strict"}},
		"memory on":       {orchdv1.Orchd_SetEnforcement_FullMethodName, &orchdv1.SetEnforcementRequest{Layer: "memory", State: "on"}},
		"plain setting":   {orchdv1.Orchd_SetConfig_FullMethodName, &orchdv1.SetConfigRequest{Values: map[string]string{configKey(t, false): "1"}}},
	} {
		if ran, err := e.interceptorLets(34, unsigned, c.method, c.req); !ran || err != nil {
			t.Errorf("%s from the unlocked terminal: ran %v, %v; want let through", name, ran, err)
		}
	}
	// A terminal with no unlock is refused all of it, as before.
	if ran, _ := e.interceptorLets(36, unsigned, orchdv1.Orchd_Relabel_FullMethodName, &orchdv1.RelabelRequest{}); ran {
		t.Fatal("a terminal with no unlock reached Relabel")
	}
	// The guard set to strict by Orch.app is not loosened back to the
	// default under an unlock; tightening further is still allowed.
	if _, err := e.s.SetEnforcement(operatorCtx, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchStrict}); err != nil {
		t.Fatal(err)
	}
	// The handler decides this one: the interceptor lets block-other-jobs
	// through, not knowing the current state.
	unlocked := e.resolvedCtx(34)
	if _, err := e.s.SetEnforcement(unlocked, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchBlockOtherJobs}); grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "D-78") {
		t.Fatalf("loosening the guard from strict under an unlock = %v", err)
	}
	if sw := switchesOf(t, e.s, operatorCtx)["guard"]; sw.GetState() != spaces.SwitchStrict {
		t.Fatalf("the guard is %s after a refused loosening", sw.GetState())
	}
	if _, err := e.s.SetEnforcement(unlocked, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchStrict}); err != nil {
		t.Fatalf("keeping the guard strict under an unlock = %v", err)
	}
	// A timed switch that reverts to strict cannot be made a lasting
	// looser one: the revert target counts while the timer runs.
	if _, err := e.s.SetEnforcement(operatorCtx, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchOff, ForSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetEnforcement(unlocked, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchBlockOtherJobs}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("replacing a timed off that reverts to strict under an unlock = %v", err)
	}
	if sw := switchesOf(t, e.s, operatorCtx)["guard"]; sw.GetState() != spaces.SwitchOff || sw.GetRevertsTo() != spaces.SwitchStrict {
		t.Fatalf("the guard's timer changed: %v", sw)
	}
	if _, err := e.s.SetEnforcement(unlocked, &orchdv1.SetEnforcementRequest{Layer: "guard", State: spaces.SwitchStrict}); err != nil {
		t.Fatalf("restoring strict under an unlock = %v", err)
	}
	// Purging docs while removing an owner stays Orch.app's (D-77), so an
	// unlock is no way around PurgeDocs.
	if _, err := e.s.RemoveDocsOwner(unlocked, &orchdv1.RemoveDocsOwnerRequest{Owner: "acme", Purge: true}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatal("an unlocked terminal purged an owner's docs")
	}
	if weakens(orchdv1.Orchd_SetEnforcement_FullMethodName, nil) == "" {
		t.Fatal("an unreadable SetEnforcement request is let through")
	}
	if refuseUnlocked(resolvedCaller{kind: callerSession}, orchdv1.Orchd_ApplySpaces_FullMethodName, nil) != nil {
		t.Fatal("refuseUnlocked decided for a session")
	}
}
