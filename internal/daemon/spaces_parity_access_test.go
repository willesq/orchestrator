package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesPaneParityAccess checks the CLI parity table's access column
// (SP-055) against orchd's rules, so the table never tells the operator a
// terminal can do what orchd keeps for Orch.app (D-71, D-78): an entry is
// "orch-app" exactly when its RPC is one an unlocked terminal is refused,
// and every non-"anyone" entry's RPC is refused to queued tasks.
func TestSpacesPaneParityAccess(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "spaces_pane_parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Actions []struct {
			Method string   `json:"method"`
			RPCs   []string `json:"rpcs"`
			Access string   `json:"access"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Actions) == 0 {
		t.Fatal("the parity table is empty")
	}
	// What each RPC is asked to do where the request decides (D-78): the
	// pane's sensitive setting, and switching a layer on.
	requests := map[string]any{
		"SetConfig":      &orchdv1.SetConfigRequest{Values: map[string]string{"sessions.command_log.retention": `"7d"`}},
		"SetEnforcement": &orchdv1.SetEnforcementRequest{Layer: "guard", State: "strict"},
	}
	for _, a := range table.Actions {
		for _, rpc := range a.RPCs {
			method := "/" + orchdv1.Orchd_ServiceDesc.ServiceName + "/" + rpc
			if _, known := readSpaceDecisions[method]; !known {
				t.Errorf("%s: %s is not an orchd RPC", a.Method, rpc)
				continue
			}
			appOnly := weakens(method, requests[rpc]) != ""
			if appOnly != (a.Access == "orch-app") {
				t.Errorf("%s: access %q, but orchd keeps %s for Orch.app under a policy: %v", a.Method, a.Access, rpc, appOnly)
			}
			if a.Access != "anyone" && taskCallable[method] {
				t.Errorf("%s: access %q, but queued tasks may call %s", a.Method, a.Access, rpc)
			}
		}
	}
}
