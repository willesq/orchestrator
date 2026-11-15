package spaces

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSessionReadsFromGuardFiles: a session's hook shows shared events of
// the spaces its pin reads, by orchd's guard files; with no policy every
// event, and with a policy it cannot read none (SP-026).
func TestSessionReadsFromGuardFiles(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	dbPath := filepath.Join(os.Getenv("ORCH_DATA_DIR"), "orch.db")
	dir := filepath.Join(t.TempDir(), "guard")
	files := GuardFiles{Dir: dir, DB: dbPath}
	if r := files.SessionReads(ctx, "s1"); !r.All || !r.Shows("") {
		t.Fatalf("nothing applied: %+v", r)
	}
	p := Policy{Spaces: []Space{
		{Name: "job", Kind: Isolated, GitHubOrgs: []string{"joborg"}, Reads: []string{"me"}},
		{Name: "other", Kind: Isolated, GitHubOrgs: []string{"otherorg"}},
		{Name: "pub", Kind: Public, GitHubOrgs: []string{"puborg"}},
		{Name: "me", Kind: Private, GitHubOrgs: []string{"meorg"}},
	}}
	write := func(s GuardSnapshot) {
		t.Helper()
		if err := WriteGuardSnapshot(dir, s); err != nil {
			t.Fatal(err)
		}
	}
	write(GuardSnapshot{Revision: 2, Policy: &p, Home: "/Users/user"})
	if err := WriteGuardPin(dir, GuardPinFile{SessionID: "s1", Space: "job", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if r := files.SessionReads(ctx, "s1"); !slices.Equal(r.Spaces, []string{"job", "me", "pub"}) || r.Shows("other") || r.Shows("") || r.Shows(Quarantine) {
		t.Errorf("job's session reads %+v", r)
	}
	if r := files.SessionReads(ctx, "unpinned"); !slices.Equal(r.Spaces, []string{"pub"}) {
		t.Errorf("an unpinned session reads %+v", r)
	}
	for name, s := range map[string]GuardSnapshot{
		"broken":  {Revision: 2, Broken: true},
		"invalid": {Revision: 2, Policy: &Policy{Spaces: []Space{{Name: "x", Kind: "nonsense"}}}},
	} {
		write(s)
		if r := files.SessionReads(ctx, "s1"); r.All || len(r.Spaces) > 0 {
			t.Errorf("%s policy reads %+v", name, r)
		}
	}
	for _, s := range []GuardSnapshot{{Revision: 0}, {Revision: 2, Policy: &Policy{}}} {
		write(s)
		if r := files.SessionReads(ctx, "s1"); !r.All {
			t.Errorf("no spaces configured reads %+v", r)
		}
	}
	// No snapshot, but orch.db applied a policy: nothing shows.
	if err := os.Remove(GuardSnapshotPath(dir)); err != nil {
		t.Fatal(err)
	}
	saveAndApply(t, openEngine(t, conn), validPolicy)
	if r := files.SessionReads(ctx, "s1"); r.All || len(r.Spaces) > 0 {
		t.Errorf("applied with no snapshot reads %+v", r)
	}
	if got := (*Resolved)(nil).ReadableFrom("job"); len(got) != 0 {
		t.Errorf("no policy reads %v", got)
	}
}
