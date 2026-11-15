package status

import (
	"context"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesSwitchLines: off with nothing kept, orch status shows no spaces
// section, as before spaces; off with a policy kept it says so; a store
// move shows its counts; turning on says nothing is enforced yet.
func TestSpacesSwitchLines(t *testing.T) {
	move := &orchdv1.SpacesStoreMove{Direction: "merge", Running: true, Collections: 3, CollectionsDone: 1, Sources: 4, Chunks: 9}
	tests := []struct {
		name  string
		sp    *orchdv1.SpacesStatus
		lines []string
	}{
		{name: "off, fresh install", sp: &orchdv1.SpacesStatus{State: "off"}},
		{name: "off, relaxed switches and sandbox ignored", sp: &orchdv1.SpacesStatus{State: "off",
			Relaxed: []*orchdv1.EnforcementSwitch{{Layer: "guard", State: "off", Relaxed: true}}, Sandbox: &orchdv1.SandboxSetting{SetAt: 1}}},
		{name: "off, policy kept", sp: &orchdv1.SpacesStatus{State: "off", AppliedRevision: 4},
			lines: []string{"switch=off/"}},
		{name: "off, merging", sp: &orchdv1.SpacesStatus{State: "off", AppliedRevision: 4, StoreMove: move},
			lines: []string{"switch=off/", "store move=merging every space's memory into one store: 1 of 3 collection(s), 4 source(s) and 9 chunk(s) moved/"}},
		{name: "turning on", sp: &orchdv1.SpacesStatus{State: "turning_on", AppliedRevision: 4,
			StoreMove: &orchdv1.SpacesStoreMove{Direction: "split", Running: true, Collections: 1, Error: "store move: a store request failed"}},
			lines: []string{"switch=turning on/warn", "store move=moving memory back into its spaces: 0 of 1 collection(s), 0 source(s) and 0 chunk(s) moved/warn",
				"policy=revision 4 applied/ok"}},
		{name: "on", sp: &orchdv1.SpacesStatus{State: "on", Configured: true, AppliedRevision: 4},
			lines: []string{"policy=revision 4 applied/ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
				return &orchdv1.StatusResponse{Spaces: tt.sp}, nil
			}
			r := Collect(context.Background(), d)
			var got []string
			found := false
			for _, s := range r.Sections {
				if s.Title != "spaces" {
					continue
				}
				found = true
				for _, l := range s.Lines {
					got = append(got, l.Name+"="+l.Value+"/"+l.Level)
				}
			}
			if tt.lines == nil {
				if found {
					t.Fatalf("spaces section shown: %v", got)
				}
				return
			}
			if strings.Join(got, "\n") != strings.Join(tt.lines, "\n") {
				t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.lines, "\n"))
			}
		})
	}
	if l, _ := storeMoveLine(&orchdv1.SpacesStoreMove{Direction: "merge", Failed: 2}); l.Level != LevelWarn || !strings.Contains(l.Detail, "2 chunk(s)") {
		t.Fatalf("failed chunks line = %+v", l)
	}
}
