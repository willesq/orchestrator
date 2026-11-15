package daemon

import "fmt"

// Each RPC registers its caller class and its space decision from the file
// of its handler, just above the handler, so features built in parallel add
// lines to their own files instead of to one shared map literal (SP-081):
//
//	// chat_search.go
//	var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SearchDirectMessages_FullMethodName, "search direct messages",
//		spaceDecision{spaceFiltered, "memory search with chat DMs kept in"}))
//
//	func (s *server) SearchDirectMessages(...)
//
// Go initializes the maps in callers.go and readspaces_registry.go before
// any package-level variable whose initializer refers to them, so every
// registration lands before orchd serves or a test runs.
// TestEveryRPCIsClassified and TestEveryRPCHasASpaceDecision check the
// filled maps against the RPCs Orchd serves, so an RPC registered nowhere
// still fails them.

// rpcRegistration is one RPC's caller class and space decision.
type rpcRegistration struct {
	method string
	// taskCallable lets queued tasks call it; otherwise action is what the
	// refusal says a task cannot do.
	taskCallable bool
	action       string
	space        spaceDecision
}

// taskRPC registers an RPC queued tasks may call.
func taskRPC(method string, space spaceDecision) rpcRegistration {
	return rpcRegistration{method: method, taskCallable: true, space: space}
}

// operatorRPC registers an RPC refused to queued tasks; action completes
// "queue tasks cannot ..." in the refusal.
func operatorRPC(method, action string, space spaceDecision) rpcRegistration {
	return rpcRegistration{method: method, action: action, space: space}
}

// rpcTables are the maps a registration fills.
type rpcTables struct {
	taskCallable    map[string]bool
	operatorActions map[string]string
	spaces          map[string]spaceDecision
}

// add records regs. It refuses a registration without a method, an
// operator RPC without an action, and an RPC already classed or decided,
// so two files cannot both claim one RPC and leave which entry wins to
// initialization order. Nothing is added when it fails.
func (t rpcTables) add(regs ...rpcRegistration) error {
	seen := map[string]bool{}
	for _, r := range regs {
		switch {
		case r.method == "":
			return fmt.Errorf("an RPC registration has no method")
		case !r.taskCallable && r.action == "":
			return fmt.Errorf("%s: an operator-only RPC needs the action its refusal names", r.method)
		case seen[r.method] || t.taskCallable[r.method] || t.operatorActions[r.method] != "":
			return fmt.Errorf("%s is already classed for callers", r.method)
		}
		if _, ok := t.spaces[r.method]; ok {
			return fmt.Errorf("%s already has a space decision", r.method)
		}
		seen[r.method] = true
	}
	for _, r := range regs {
		if r.taskCallable {
			t.taskCallable[r.method] = true
		} else {
			t.operatorActions[r.method] = r.action
		}
		t.spaces[r.method] = r.space
	}
	return nil
}

// registerRPCs adds regs to orchd's caller and space-decision maps. It is
// called from package-level variable initializers, before orchd serves, and
// panics on a conflicting registration so the mistake stops every test and
// the daemon at start rather than changing a guard silently.
func registerRPCs(regs ...rpcRegistration) struct{} {
	t := rpcTables{taskCallable: taskCallable, operatorActions: operatorActions, spaces: readSpaceDecisions}
	if err := t.add(regs...); err != nil {
		panic("orchd RPC registry: " + err.Error())
	}
	return struct{}{}
}
