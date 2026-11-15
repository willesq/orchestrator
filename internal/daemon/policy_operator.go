package daemon

import (
	"log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Queued tasks are refused every RPC in operatorActions (callers.go), and
// since D-71 so is a caller that is the operator only by its process tree
// while a spaces policy is applied. Interactive harness sessions still reach
// most of them, since sessions dispatch, ingest and follow their tasks.
// The settings below change what every later session may run or where orch
// keeps its data, so under a policy an agent session changing them would
// step outside its space (D-74, settling RC-710). While a policy is applied
// they are the operator's alone: only Orch.app may call them (D-71). With no
// policy nothing changes (INV-10). See docs/internals/daemon.md "Operator
// evidence".

// policyOperatorOnly lists the RPCs only Orch.app may call while a spaces
// policy is applied, with what each would do, for the refusal. SetConfig is
// listed only for Sensitive keys (settingsUnderPolicy).
var policyOperatorOnly = map[string]string{
	orchdv1.Orchd_SetConfig_FullMethodName:         "change orch's sensitive settings",
	orchdv1.Orchd_SetSettingEnabled_FullMethodName: "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	orchdv1.Orchd_AddSetting_FullMethodName:        "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	orchdv1.Orchd_RemoveSetting_FullMethodName:     "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	orchdv1.Orchd_InstallSafetyNet_FullMethodName:  "install or upgrade cc-safety-net",
	orchdv1.Orchd_Restart_FullMethodName:           "restart orchd subsystems",
	orchdv1.Orchd_LandPR_FullMethodName:            "push proposed commits",
	orchdv1.Orchd_UndoPRLanding_FullMethodName:     "push proposed commits",
	orchdv1.Orchd_DiscardPRLanding_FullMethodName:  "decide PR landings",
}

// settingsUnderPolicy returns what method would do when it is the
// operator's alone under a policy, or "" when it is not. A SetConfig that
// sets no Sensitive key stays open to sessions, as before.
func settingsUnderPolicy(method string, req any) string {
	action := policyOperatorOnly[method]
	if action == "" || method != orchdv1.Orchd_SetConfig_FullMethodName {
		return action
	}
	r, ok := req.(*orchdv1.SetConfigRequest)
	if !ok {
		// Reason: a request orchd cannot read might set anything, so it
		// fails closed.
		return action
	}
	for key := range r.GetValues() {
		if f, known := config.FieldFor(key); known && f.Sensitive {
			return action
		}
	}
	return ""
}

// refuseSettings refuses method to every caller but Orch.app while a spaces
// policy is applied, when method is one of policyOperatorOnly (D-74). Each
// refusal is logged with the caller's kind and ID.
func (r *callerResolver) refuseSettings(c resolvedCaller, method string, req any) error {
	action := settingsUnderPolicy(method, req)
	if action == "" || (c.kind == callerOperator && c.operatorBy != "") {
		return nil
	}
	applied := c.evidenceNeeded
	if c.kind != callerOperator {
		// Reason: only an operator's resolution reads the policy, so
		// every other caller reads it here, and only for these RPCs.
		applied = r.needEvidence()
	}
	if !applied {
		return nil
	}
	log.Printf("orchd: refused %s from %s: only Orch.app can call it while a spaces policy is applied", rpcName(method), readerLabel(c))
	return grpcstatus.Errorf(codes.PermissionDenied,
		"only Orch.app can %s while a spaces policy is applied (D-74); ask the user to make the change in Orch.app", action)
}
