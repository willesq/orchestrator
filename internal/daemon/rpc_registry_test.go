package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
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
// call or another file's, is refused and adds nothing, as
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
// an RPC another file already registered.
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
// file but not served (renamed or removed from orchd.proto) is caught by
// the served-RPC checks.
func TestRegisteredRPCMissingFromServiceFails(t *testing.T) {
	tables := copyOfOrchdTables()
	const gone = "/orchd.v1.Orchd/NoLongerServed"
	if err := tables.add(operatorRPC(gone, "do something", spaceDecision{spaceNone, "a write"})); err != nil {
		t.Fatal(err)
	}
	served := servedMethods()
	var unserved []string
	for name := range tables.spaces {
		if !served[name] {
			unserved = append(unserved, name)
		}
	}
	if len(unserved) != 1 || unserved[0] != gone {
		t.Fatalf("decisions naming an RPC Orchd does not serve = %v, want the one registered", unserved)
	}
}

// TestUnregisteredRPCFailsTheSpaceCheck is SP-081.1.a3: with one served
// RPC's registration left out, the check TestEveryRPCHasASpaceDecision runs
// names that RPC and no other.
func TestUnregisteredRPCFailsTheSpaceCheck(t *testing.T) {
	tables := copyOfOrchdTables()
	left := orchdv1.Orchd_RetagSource_FullMethodName
	delete(tables.spaces, left)
	if missing := missingDecisions(servedMethods(), tables.spaces); len(missing) != 1 || missing[0] != left {
		t.Fatalf("RPCs without a decision = %v, want %s", missing, left)
	}
	if missing := missingDecisions(servedMethods(), readSpaceDecisions); len(missing) != 0 {
		t.Fatalf("orchd's own registry misses %v", missing)
	}
}

// copyOfOrchdTables copies the maps the registrations filled.
func copyOfOrchdTables() rpcTables {
	t := emptyTables()
	maps.Copy(t.taskCallable, taskCallable)
	maps.Copy(t.operatorActions, operatorActions)
	maps.Copy(t.spaces, readSpaceDecisions)
	return t
}

// TestEachRPCRegistersBesideItsHandler is SP-081.1.a1: every RPC Orchd
// serves is registered exactly once, from the file that holds its handler,
// and the maps the registrations fill are declared empty, so no shared map
// literal lists RPCs.
func TestEachRPCRegistersBesideItsHandler(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	registeredIn := map[string][]string{}
	handlerIn := map[string]string{}
	for _, f := range pkgs["daemon"].Files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if star, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
						if id, ok := star.X.(*ast.Ident); ok && id.Name == "server" {
							handlerIn[d.Name.Name] = file
						}
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						if lit, ok := v.(*ast.CompositeLit); ok && len(lit.Elts) > 0 {
							switch vs.Names[i].Name {
							case "taskCallable", "operatorActions", "readSpaceDecisions":
								t.Errorf("%s lists RPCs in %s; register them beside their handlers", vs.Names[i].Name, file)
							}
						}
						ast.Inspect(v, func(n ast.Node) bool {
							sel, ok := n.(*ast.SelectorExpr)
							if ok && strings.HasPrefix(sel.Sel.Name, "Orchd_") && strings.HasSuffix(sel.Sel.Name, "_FullMethodName") {
								if call, ok := v.(*ast.CallExpr); ok && isIdent(call.Fun, "registerRPCs") {
									name := strings.TrimSuffix(strings.TrimPrefix(sel.Sel.Name, "Orchd_"), "_FullMethodName")
									registeredIn[name] = append(registeredIn[name], file)
								}
							}
							return true
						})
					}
				}
			}
		}
	}
	for full := range servedMethods() {
		name := full[strings.LastIndex(full, "/")+1:]
		files := registeredIn[name]
		switch {
		case len(files) != 1:
			t.Errorf("%s is registered in %v, want once", name, files)
		case files[0] != handlerIn[name]:
			t.Errorf("%s is registered in %s, but its handler is in %s", name, files[0], handlerIn[name])
		}
	}
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
