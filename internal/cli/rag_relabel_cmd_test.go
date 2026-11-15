package cli

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// relabelResponse is a dry run's answer over a store holding acme's and
// globex's content.
func relabelResponse(apply bool) *orchdv1.RelabelResponse {
	return &orchdv1.RelabelResponse{Revision: 3, Total: 120, Stale: 40, Changed: 42, Moved: 30, Failed: 2, Applied: apply,
		Transitions: []*orchdv1.RelabelTransition{
			{From: "unplaced", To: "acme", Chunks: 25},
			{From: "unplaced", To: "quarantine", Chunks: 5},
			{From: "acme", To: "globex", Chunks: 8},
		}}
}

// TestRAGRelabelDryRunPrintsOnlyCountsAndSpaceNames: SP-027.1.a4 through the
// command. 'orch rag relabel --dry-run' asks orchd not to apply, and prints
// counts and space names only: every word is a fixed word of the report or a
// space name.
func TestRAGRelabelDryRunPrintsOnlyCountsAndSpaceNames(t *testing.T) {
	saved := fakeRelabel
	t.Cleanup(func() { fakeRelabel = saved })
	var applied []bool
	fakeRelabel = func(req *orchdv1.RelabelRequest) (*orchdv1.RelabelResponse, error) {
		applied = append(applied, req.GetApply())
		return relabelResponse(req.GetApply()), nil
	}
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "rag", "relabel", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"acme": true, "globex": true, "quarantine": true, "unplaced": true}
	for _, w := range strings.Fields("policy revision chunk s stale would change of them moved to another collection " +
		"could not be placed and stay run again retry dry without relabel") {
		allowed[w] = true
	}
	for _, w := range regexp.MustCompile(`[A-Za-z_]+`).FindAllString(out, -1) {
		if !allowed[w] {
			t.Fatalf("dry-run output has %q, neither a count nor a space name:\n%s", w, out)
		}
	}
	for _, want := range []string{"policy revision 3: 120 chunk(s), 40 stale", "unplaced -> acme: 25 chunk(s)",
		"acme -> globex: 8 chunk(s)", "would change 42 chunk(s), 30 of them moved", "2 chunk(s) could not be placed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if out, err := run(cf, "", "rag", "relabel"); err != nil || !strings.Contains(out, "changed 42 chunk(s)") || strings.Contains(out, "dry run") {
		t.Fatalf("apply = %s, %v", out, err)
	}
	if len(applied) != 2 || applied[0] || !applied[1] {
		t.Fatalf("apply flags = %v", applied)
	}
}

func TestRAGRelabelFailures(t *testing.T) {
	saved := fakeRelabel
	t.Cleanup(func() { fakeRelabel = saved })
	cf := factory(&fakeClient{}, nil)
	fakeRelabel = func(*orchdv1.RelabelRequest) (*orchdv1.RelabelResponse, error) {
		r := relabelResponse(true)
		r.Error = "relabel: a store request failed"
		r.Transitions[0].To = "acme\x1b[2J"
		return r, nil
	}
	out, err := run(cf, "", "rag", "relabel")
	if err == nil || !strings.Contains(out, "run it again") || strings.Contains(out, "\x1b") {
		t.Fatalf("partial = %q, %v", out, err)
	}
	fakeRelabel = func(*orchdv1.RelabelRequest) (*orchdv1.RelabelResponse, error) { return nil, errors.New("unavailable") }
	if _, err := run(cf, "", "rag", "relabel", "--dry-run"); err == nil {
		t.Fatal("RPC error swallowed")
	}
}
