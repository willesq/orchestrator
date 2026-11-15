package daemon

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// emptyTables returns registry maps holding nothing.
func emptyTables() rpcTables {
	return rpcTables{taskCallable: map[string]bool{}, operatorActions: map[string]string{}, spaces: map[string]spaceDecision{}}
}

// TestRegistrationFillsEveryTable: a task RPC lands in taskCallable, an
// operator RPC in operatorActions with its action, and both get their
// space decision.
func TestRegistrationFillsEveryTable(t *testing.T) {
	tables := emptyTables()
	read := spaceDecision{spaceOwn, "the caller's own"}
	write := spaceDecision{spaceNone, "a write"}
	if err := tables.add(taskRPC("/orchd.v1.Orchd/A", read), operatorRPC("/orchd.v1.Orchd/B", "do b", write)); err != nil {
		t.Fatal(err)
	}
	if !tables.taskCallable["/orchd.v1.Orchd/A"] || tables.operatorActions["/orchd.v1.Orchd/A"] != "" {
		t.Errorf("A: taskCallable %v, action %q", tables.taskCallable["/orchd.v1.Orchd/A"], tables.operatorActions["/orchd.v1.Orchd/A"])
	}
	if tables.taskCallable["/orchd.v1.Orchd/B"] || tables.operatorActions["/orchd.v1.Orchd/B"] != "do b" {
		t.Errorf("B: taskCallable %v, action %q", tables.taskCallable["/orchd.v1.Orchd/B"], tables.operatorActions["/orchd.v1.Orchd/B"])
	}
	if tables.spaces["/orchd.v1.Orchd/A"] != read || tables.spaces["/orchd.v1.Orchd/B"] != write {
		t.Errorf("space decisions = %v", tables.spaces)
	}
}

// TestRegistrationRefusesConflicts: a second claim on an RPC, from the same
// call, another feature or the shared maps, is refused and adds nothing, as
// is a registration with no method or an operator RPC with no action.
func TestRegistrationRefusesConflicts(t *testing.T) {
	d := spaceDecision{spaceNone, "a write"}
	tests := []struct {
		name    string
		prior   func(rpcTables)
		regs    []rpcRegistration
		wantErr string
	}{
		{"no method", nil, []rpcRegistration{taskRPC("", d)}, "no method"},
		{"operator without action", nil, []rpcRegistration{operatorRPC("/x/A", "", d)}, "needs the action"},
		{"same call twice", nil, []rpcRegistration{taskRPC("/x/A", d), operatorRPC("/x/A", "do a", d)}, "already classed"},
		{"already task callable", func(t rpcTables) { t.taskCallable["/x/A"] = true }, []rpcRegistration{taskRPC("/x/A", d)}, "already classed"},
		{"already operator only", func(t rpcTables) { t.operatorActions["/x/A"] = "do a" }, []rpcRegistration{taskRPC("/x/A", d)}, "already classed"},
		{"already decided", func(t rpcTables) { t.spaces["/x/A"] = d }, []rpcRegistration{taskRPC("/x/A", d)}, "already has a space decision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tables := emptyTables()
			if tt.prior != nil {
				tt.prior(tables)
			}
			before := len(tables.taskCallable) + len(tables.operatorActions) + len(tables.spaces)
			// A valid registration first in the call must not be kept either.
			regs := append([]rpcRegistration{taskRPC("/x/Valid", d)}, tt.regs...)
			err := tables.add(regs...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if after := len(tables.taskCallable) + len(tables.operatorActions) + len(tables.spaces); after != before {
				t.Errorf("a refused registration added %d entries", after-before)
			}
		})
	}
}

// TestRegisterRPCsFillsOrchdsMaps: registerRPCs writes the maps the guards
// and TestEveryRPCHasASpaceDecision read, and panics, changing nothing, on
// an RPC the shared maps already class.
func TestRegisterRPCsFillsOrchdsMaps(t *testing.T) {
	const added = "/orchd.v1.Orchd/RegistryTestRPC"
	t.Cleanup(func() {
		delete(operatorActions, added)
		delete(readSpaceDecisions, added)
	})
	registerRPCs(operatorRPC(added, "test the registry", spaceDecision{spaceNone, "a test"}))
	if operatorActions[added] != "test the registry" || readSpaceDecisions[added].kind != spaceNone {
		t.Fatalf("registered RPC missing: action %q, decision %+v", operatorActions[added], readSpaceDecisions[added])
	}

	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), orchdv1.Orchd_Search_FullMethodName) {
			t.Errorf("recover() = %v, want a panic naming Search", r)
		}
		if readSpaceDecisions[orchdv1.Orchd_Search_FullMethodName].kind != spaceFiltered {
			t.Errorf("a refused registration changed Search's decision")
		}
	}()
	registerRPCs(taskRPC(orchdv1.Orchd_Search_FullMethodName, spaceDecision{spaceNone, "wrong"}))
}

// TestRegisteredRPCMissingFromServiceFails: an RPC registered from a feature
// file but not served (renamed or removed from orchd.proto) fails the same
// checks as one missing from the shared maps.
func TestRegisteredRPCMissingFromServiceFails(t *testing.T) {
	served := servedMethods()
	tables := rpcTables{taskCallable: map[string]bool{}, operatorActions: map[string]string{}, spaces: map[string]spaceDecision{}}
	for name, ok := range taskCallable {
		tables.taskCallable[name] = ok
	}
	for name, a := range operatorActions {
		tables.operatorActions[name] = a
	}
	for name, d := range readSpaceDecisions {
		tables.spaces[name] = d
	}
	const gone = "/orchd.v1.Orchd/NoLongerServed"
	if err := tables.add(operatorRPC(gone, "do something", spaceDecision{spaceNone, "a write"})); err != nil {
		t.Fatal(err)
	}
	unserved := 0
	for name := range tables.spaces {
		if !served[name] {
			unserved++
		}
	}
	if unserved != 1 {
		t.Fatalf("%d decisions name an RPC Orchd does not serve, want the one registered", unserved)
	}
}
