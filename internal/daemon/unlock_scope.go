package daemon

import (
	"context"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// An unlocked terminal (SP-077) is the operator for reads and operator
// tasks such as relabel and retag, but RPCs that weaken protection stay
// Orch.app's alone even under an unlock (D-78, settling RC-1150): applying
// a policy, switching an enforcement layer to a relaxing state, changing the
// sandbox setting or the outbound rules, and SP-078's settings RPCs
// (policy_operator.go). Reason: those changes outlast the unlock, so a
// mistaken confirmation must not turn into a lasting weaker policy.

// unlockWeakens lists the RPCs refused to an unlocked terminal, with what
// each would do; SetEnforcement only when its state relaxes the layer, and
// SP-078's settings as settingsUnderPolicy decides.
var unlockWeakens = map[string]string{
	orchdv1.Orchd_ApplySpaces_FullMethodName:        "apply a spaces policy",
	orchdv1.Orchd_SetEnforcement_FullMethodName:     "switch enforcement off",
	orchdv1.Orchd_SetSandboxSetting_FullMethodName:  "change the sandbox setting",
	orchdv1.Orchd_ChangeOutboundRule_FullMethodName: "change the outbound rules",
	orchdv1.Orchd_SetSpaceTerms_FullMethodName:      "change a space's terms",
}

// weakens returns what method would do when it weakens protection under
// D-78, or "" when an unlocked terminal may call it.
func weakens(method string, req any) string {
	if action := settingsUnderPolicy(method, req); action != "" {
		return action
	}
	action := unlockWeakens[method]
	if method != orchdv1.Orchd_SetEnforcement_FullMethodName {
		return action
	}
	r, ok := req.(*orchdv1.SetEnforcementRequest)
	if !ok {
		// Reason: a request orchd cannot read might switch anything off.
		return action
	}
	l, err := spaces.ParseLayer(r.GetLayer())
	// Reason: compared as the handler would read it, so "OFF" or " off"
	// does not pass for a tightening.
	if err != nil || l.Relaxed(strings.ToLower(strings.TrimSpace(r.GetState()))) {
		return action
	}
	return ""
}

// strictness ranks a layer's states, loosest first: the guard's off, log,
// block-other-jobs and strict; every other layer's off and on.
func strictness(state string) int {
	switch state {
	case spaces.SwitchOff:
		return 0
	case spaces.SwitchLog:
		return 1
	case spaces.SwitchStrict:
		return 3
	}
	return 2 // on, block-other-jobs
}

// refuseUnlockedLoosening refuses an unlocked terminal a switch to a state
// looser than the layer's current one, e.g. the guard from strict back to
// block-other-jobs, which the interceptor cannot see without the current
// state (D-78). While a timer runs, the state it reverts to counts too, since
// a new switch replaces the timer: a timed guard off that reverts to strict
// cannot be turned into a lasting block-other-jobs. Orch.app and every other
// caller are decided elsewhere.
func refuseUnlockedLoosening(ctx context.Context, cur spaces.Switch, state string) error {
	c := callerOf(ctx)
	if c.kind != callerOperator || c.operatorBy != operatorUnlock {
		return nil
	}
	floor := strictness(cur.State)
	if !cur.Until.IsZero() {
		floor = max(floor, strictness(cur.Previous))
	}
	if strictness(strings.ToLower(strings.TrimSpace(state))) >= floor {
		return nil
	}
	return grpcstatus.Error(codes.PermissionDenied,
		"only Orch.app can loosen an enforcement switch, even under an unlock (D-78); make the change in Orch.app")
}

// refuseUnlocked refuses method to a caller whose evidence is an unlock
// when it weakens protection (D-78). Orch.app and every other caller are
// decided elsewhere.
func refuseUnlocked(c resolvedCaller, method string, req any) error {
	if c.kind != callerOperator || c.operatorBy != operatorUnlock {
		return nil
	}
	action := weakens(method, req)
	if action == "" {
		return nil
	}
	if unlockLogs.allow(time.Now()) {
		log.Printf("orchd: refused %s from an unlocked terminal: only Orch.app may weaken protection (D-78)", rpcName(method))
	}
	return grpcstatus.Errorf(codes.PermissionDenied,
		"only Orch.app can %s, even under an unlock (D-78); make the change in Orch.app", action)
}
