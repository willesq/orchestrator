package status

import (
	"bytes"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// sectionLines is the spaces section of resp as "name=value/level" lines.
func sectionLines(t *testing.T, resp *orchdv1.StatusResponse) []string {
	t.Helper()
	sec, ok := spacesSection(resp, nil)
	if !ok {
		t.Fatal("no spaces section")
	}
	var out []string
	for _, l := range sec.Lines {
		out = append(out, l.Name+"="+l.Value+"/"+l.Level)
	}
	return out
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestSpacesSectionShowsCounts is SP-031.1.a1: the operator's status shows
// the spaces, chunks per space, conflicts, drift and relabel progress
// (quarantine is SP-028's line); a session's shows only the spaces orchd
// counted for it and says conflicts are the operator's.
func TestSpacesSectionShowsCounts(t *testing.T) {
	op := &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 4,
		Counts: &orchdv1.SpacesCounts{EverySpace: true, Chunks: 400, Stale: 100, Conflicts: 2, ConflictsCounted: true,
			Spaces: []*orchdv1.SpaceCount{
				{Space: "acme", Kind: "isolated", Chunks: 250, Stale: 90},
				{Space: "oss", Kind: "public", Chunks: 120},
			}}}}
	got := sectionLines(t, op)
	for _, want := range []string{
		"spaces=2/ok",
		"space acme=250 chunk(s)/ok",
		"space oss=120 chunk(s)/ok",
		"relabel=300 of 400 chunk(s) labelled (75%)/warn",
		"conflicts=2 session(s)/warn",
		"drift=none/ok",
	} {
		if !has(got, want) {
			t.Fatalf("operator lines %v lack %q", got, want)
		}
	}
	sec, _ := spacesSection(op, nil)
	var buf bytes.Buffer
	Report{Sections: []Section{sec}}.Render(&buf)
	for _, want := range []string{"isolated, 90 not yet relabelled", "orch rag relabel", "every space of the policy"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, buf.String())
		}
	}

	agent := &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 4, Drift: []string{"draft"}, DraftRevision: 5,
		Counts: &orchdv1.SpacesCounts{Chunks: 120, Spaces: []*orchdv1.SpaceCount{{Space: "oss", Kind: "public", Chunks: 120}}}}}
	got = sectionLines(t, agent)
	for _, want := range []string{
		"spaces=1 readable here/ok",
		"space oss=120 chunk(s)/ok",
		"relabel=done/ok",
		"conflicts=shown to the operator/ok",
		"drift=draft revision 5 not applied/warn",
	} {
		if !has(got, want) {
			t.Fatalf("agent lines %v lack %q", got, want)
		}
	}
	if has(got, "drift=none/ok") || has(got, "space acme=250 chunk(s)/ok") {
		t.Fatalf("agent lines %v", got)
	}
}

// TestSpacesSectionCountEdges: no counts, a failed count, an empty store,
// and a space name that would move the cursor.
func TestSpacesSectionCountEdges(t *testing.T) {
	base := func(c *orchdv1.SpacesCounts) *orchdv1.StatusResponse {
		return &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 1, Counts: c}}
	}
	if got := sectionLines(t, base(nil)); has(got, "drift=none/ok") || len(got) != 1 {
		t.Fatalf("no counts: %v", got)
	}
	if got := sectionLines(t, base(&orchdv1.SpacesCounts{Error: "the store could not be counted"})); !has(got, "counts=unavailable/warn") {
		t.Fatalf("failed count: %v", got)
	}
	got := sectionLines(t, base(&orchdv1.SpacesCounts{EverySpace: true, ConflictsCounted: true,
		Spaces: []*orchdv1.SpaceCount{{Space: "a\x1b[2Jb", Kind: "private"}}}))
	for _, want := range []string{"relabel=nothing stored/ok", "conflicts=none/ok", "space a [2Jb=0 chunk(s)/ok"} {
		if !has(got, want) {
			t.Fatalf("lines %v lack %q", got, want)
		}
	}
}
