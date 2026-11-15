package daemon

import (
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// servedMethods lists every RPC Orchd serves, by full method name.
func servedMethods() map[string]bool {
	out := map[string]bool{}
	prefix := "/" + orchdv1.Orchd_ServiceDesc.ServiceName + "/"
	for _, m := range orchdv1.Orchd_ServiceDesc.Methods {
		out[prefix+m.MethodName] = true
	}
	for _, m := range orchdv1.Orchd_ServiceDesc.Streams {
		out[prefix+m.StreamName] = true
	}
	return out
}

// TestEveryRPCHasASpaceDecision is SP-025.2.a1: an RPC with no entry in
// the space-decision registry fails, as one missing from taskCallable and
// operatorActions does, so no RPC returns stored content without a decided
// scope. Every entry names a served RPC and a known kind with a reason, and
// the filtered and operator-only kinds agree with the guards.
func TestEveryRPCHasASpaceDecision(t *testing.T) {
	served := servedMethods()
	for _, name := range missingDecisions(served, readSpaceDecisions) {
		t.Errorf("%s has no entry in readSpaceDecisions: decide how the content it returns is kept within the caller's spaces", name)
	}
	kinds := map[string]bool{spaceFiltered: true, spaceOperator: true, spaceOwn: true, spaceRecords: true, spaceNone: true}
	filtered := 0
	for name, d := range readSpaceDecisions {
		switch {
		case !served[name]:
			t.Errorf("readSpaceDecisions names %s, which Orchd does not serve", name)
		case !kinds[d.kind] || d.why == "":
			t.Errorf("%s: decision %+v needs a known kind and a reason", name, d)
		case d.kind == spaceOperator && operatorActions[name] == "":
			// refuseNonOperator refuses tasks first, through operatorActions.
			t.Errorf("%s is operator-only but queued tasks may call it", name)
		case d.kind == spaceFiltered:
			filtered++
		}
	}
	if filtered != 4 {
		t.Errorf("%d filtered memory reads, want Search, ListSources, GetDocument and ListDocs", filtered)
	}
}

// missingDecisions lists the served RPCs with no space decision.
func missingDecisions(served map[string]bool, decisions map[string]spaceDecision) []string {
	var out []string
	for name := range served {
		if _, ok := decisions[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// TestRegistryCatchesAMissingRPC: the check above fails for an RPC added
// without a decision.
func TestRegistryCatchesAMissingRPC(t *testing.T) {
	const added = "/orchd.v1.Orchd/NewContentRPC"
	served := servedMethods()
	served[added] = true
	if missing := missingDecisions(served, readSpaceDecisions); len(missing) != 1 || missing[0] != added {
		t.Fatalf("RPCs without a decision = %v, want the one added", missing)
	}
}
