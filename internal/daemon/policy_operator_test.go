package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// settingsEnv resolves callers of every kind against session pins, with a
// switch for whether a spaces policy is applied; asks counts how often the
// policy was read.
type settingsEnv struct {
	r       *callerResolver
	applied bool
	asks    int
	callers map[string]context.Context
}

func newSettingsEnv(t *testing.T) *settingsEnv {
	t.Helper()
	r, rt, _ := pinnedResolver(t)
	e := &settingsEnv{r: r, applied: true}
	r.needsEvidence = func() bool { e.asks++; return e.applied }
	ot := newOperatorTree()
	e.callers = map[string]context.Context{
		// A CLI in claude 400's pinned session, and one in claude 401,
		// which holds no pin.
		"session":          rt.connOf(950),
		"unpinned session": rt.connOf(970),
		"queued task":      callerCtx(callerAuth{pid: 990, spawned: true}),
		"terminal":         callerCtx(ot.creds(32, fakeVerifier()).identify(signedConn{err: errors.New("not signed")})),
		"detached process": callerCtx(ot.creds(40, fakeVerifier()).identify(nil)),
		"Orch.app":         callerCtx(ot.creds(50, fakeVerifier()).identify(appSig("/Applications/Orch.app", codeRuntime, genuineCDHash))),
	}
	return e
}

// call runs method from caller through the resolver's interceptor and
// reports whether the handler ran.
func (e *settingsEnv) call(caller, method string, req any) (bool, error) {
	ran := false
	_, err := e.r.unary(e.callers[caller], req, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { ran = true; return nil, nil })
	return ran, err
}

func (e *settingsEnv) stream(caller, method string) (bool, error) {
	ran := false
	err := e.r.stream(nil, fakeStream{ctx: e.callers[caller]}, &grpc.StreamServerInfo{FullMethod: method},
		func(any, grpc.ServerStream) error { ran = true; return nil })
	return ran, err
}

// configKey returns a key clients may set that is (or is not) Sensitive.
func configKey(t *testing.T, sensitive bool) string {
	t.Helper()
	for _, f := range config.Fields {
		if !f.Hidden && f.Sensitive == sensitive {
			return f.Key
		}
	}
	t.Fatalf("no setting with Sensitive %v", sensitive)
	return ""
}

// settingsRPCs are the RPCs SP-078.1.a1 names, with a request each.
func settingsRPCs(t *testing.T) map[string]any {
	return map[string]any{
		orchdv1.Orchd_SetConfig_FullMethodName: &orchdv1.SetConfigRequest{
			Values: map[string]string{configKey(t, false): "1", configKey(t, true): `"x"`}, ConfirmSensitive: true},
		orchdv1.Orchd_SetSettingEnabled_FullMethodName: &orchdv1.SetSettingEnabledRequest{},
		orchdv1.Orchd_AddSetting_FullMethodName:        &orchdv1.AddSettingRequest{},
		orchdv1.Orchd_RemoveSetting_FullMethodName:     &orchdv1.RemoveSettingRequest{},
		orchdv1.Orchd_InstallSafetyNet_FullMethodName:  &orchdv1.InstallSafetyNetRequest{},
		orchdv1.Orchd_Restart_FullMethodName:           &orchdv1.RestartRequest{},
		orchdv1.Orchd_LandPR_FullMethodName:            &orchdv1.LandPRRequest{},
		orchdv1.Orchd_UndoPRLanding_FullMethodName:     &orchdv1.PRLandingRequest{},
		orchdv1.Orchd_DiscardPRLanding_FullMethodName:  &orchdv1.DiscardPRLandingRequest{},
	}
}

// TestSettingsAreTheOperatorsUnderAPolicy is SP-078.1.a1 (D-74, RC-710):
// while a spaces policy is applied, SetConfig with a Sensitive key, the
// harness and cc-safety-net settings RPCs, Restart and PR landing are
// refused to every caller but Orch.app, interactive harness sessions
// included, before their handler runs, and each refusal is logged.
func TestSettingsAreTheOperatorsUnderAPolicy(t *testing.T) {
	logs := captureReadLog(t)
	e := newSettingsEnv(t)
	rpcs := settingsRPCs(t)
	if len(rpcs) != len(policyOperatorOnly) {
		t.Fatalf("the test covers %d RPCs, policyOperatorOnly lists %d", len(rpcs), len(policyOperatorOnly))
	}
	for method, req := range rpcs {
		t.Run(rpcName(method), func(t *testing.T) {
			for _, who := range []string{"session", "unpinned session", "queued task", "terminal", "detached process"} {
				before := logs.String()
				ran, err := e.call(who, method, req)
				if ran || grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "only Orch.app can") {
					t.Errorf("from a %s: ran %v, %v; want refused", who, ran, err)
				}
				if logged := strings.TrimPrefix(logs.String(), before); !strings.Contains(logged, "refused "+rpcName(method)) {
					t.Errorf("refusal to a %s not logged: %q", who, logged)
				}
			}
			if !strings.Contains(logs.String(), "refused "+rpcName(method)+" from session sess-a: only Orch.app") {
				t.Errorf("the session's refusal does not name it:\n%s", logs.String())
			}
			if ran, err := e.call("Orch.app", method, req); !ran || err != nil {
				t.Errorf("from Orch.app: ran %v, %v", ran, err)
			}
		})
	}
	// The stream interceptor holds the same line.
	if ran, err := e.stream("session", orchdv1.Orchd_Restart_FullMethodName); ran || grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("Restart as a stream from a session: ran %v, %v", ran, err)
	}
	// A SetConfig orchd cannot read might set a Sensitive key.
	if ran, err := e.call("session", orchdv1.Orchd_SetConfig_FullMethodName, nil); ran || grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("an unreadable SetConfig from a session: ran %v, %v", ran, err)
	}
	// The policy check is the real one: under an applied policy a session
	// is refused, and a session never needs Orch.app's evidence otherwise.
	re := newReadEnv(t)
	withPolicy := &callerResolver{needsEvidence: policyApplied(re.s.spaces)}
	if err := withPolicy.refuseSettings(resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "s"}},
		orchdv1.Orchd_Restart_FullMethodName, nil); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("Restart from a session under an applied policy = %v", err)
	}
}

// TestSessionsKeepTheirRPCsUnderAPolicy is SP-078.1.a2: under a policy,
// sessions still ingest, dispatch, follow their tasks and change settings
// that are not Sensitive; with no policy every RPC behaves as before
// (INV-10), the settings RPCs included.
func TestSessionsKeepTheirRPCsUnderAPolicy(t *testing.T) {
	e := newSettingsEnv(t)
	sessionRPCs := map[string]any{
		orchdv1.Orchd_Ingest_FullMethodName:     &orchdv1.IngestRequest{},
		orchdv1.Orchd_Dispatch_FullMethodName:   &orchdv1.DispatchRequest{},
		orchdv1.Orchd_ListTasks_FullMethodName:  &orchdv1.ListTasksRequest{},
		orchdv1.Orchd_GetTask_FullMethodName:    &orchdv1.GetTaskRequest{},
		orchdv1.Orchd_RateTask_FullMethodName:   &orchdv1.RateTaskRequest{},
		orchdv1.Orchd_RetryTask_FullMethodName:  &orchdv1.RetryTaskRequest{},
		orchdv1.Orchd_CancelTask_FullMethodName: &orchdv1.CancelTaskRequest{},
		orchdv1.Orchd_Search_FullMethodName:     &orchdv1.SearchRequest{},
		orchdv1.Orchd_SetConfig_FullMethodName: &orchdv1.SetConfigRequest{
			Values: map[string]string{configKey(t, false): "1"}},
	}
	for _, who := range []string{"session", "unpinned session"} {
		for method, req := range sessionRPCs {
			if ran, err := e.call(who, method, req); !ran || err != nil {
				t.Errorf("under a policy %s from a %s: ran %v, %v", rpcName(method), who, ran, err)
			}
		}
		if ran, err := e.stream(who, orchdv1.Orchd_WatchTasks_FullMethodName); !ran || err != nil {
			t.Errorf("under a policy WatchTasks from a %s: ran %v, %v", who, ran, err)
		}
	}
	// Reason: the policy is read only for the RPCs it can refuse, so other
	// calls from sessions cost nothing more.
	if e.asks != 0 {
		t.Errorf("the policy was read %d times for RPCs it cannot refuse", e.asks)
	}

	e.applied = false
	for method, req := range settingsRPCs(t) {
		for _, who := range []string{"session", "unpinned session", "terminal", "detached process", "Orch.app"} {
			if ran, err := e.call(who, method, req); !ran || err != nil {
				t.Errorf("with no policy %s from a %s: ran %v, %v", rpcName(method), who, ran, err)
			}
		}
	}
	if ran, err := e.stream("session", orchdv1.Orchd_Restart_FullMethodName); !ran || err != nil {
		t.Errorf("with no policy Restart as a stream from a session: ran %v, %v", ran, err)
	}
	var none *callerResolver
	if err := none.refuseSettings(resolvedCaller{kind: callerSession}, orchdv1.Orchd_Restart_FullMethodName, nil); err != nil {
		t.Errorf("a resolver with no policy check refused Restart: %v", err)
	}
}
