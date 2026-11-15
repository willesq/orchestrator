package status

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesSectionCountsQuarantine: SP-028's counts in orch status, with
// no source named.
func TestSpacesSectionCountsQuarantine(t *testing.T) {
	for _, tc := range []struct {
		q    *orchdv1.QuarantineCounts
		want string // "" for no line
	}{
		{nil, ""},
		{&orchdv1.QuarantineCounts{}, "empty"},
		{&orchdv1.QuarantineCounts{Sources: 3, Chunks: 40}, "3 source(s), 40 chunk(s)"},
	} {
		resp := &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 4, Quarantine: tc.q}}
		sec, ok := spacesSection(resp, nil)
		if !ok {
			t.Fatal("no spaces section")
		}
		var got *Line
		for i, l := range sec.Lines {
			if l.Name == "quarantine" {
				got = &sec.Lines[i]
			}
		}
		switch {
		case tc.want == "" && got != nil:
			t.Fatalf("unexpected line %+v", *got)
		case tc.want != "" && (got == nil || got.Value != tc.want || got.Level != LevelOK):
			t.Fatalf("line = %+v, want %q", got, tc.want)
		case tc.want != "" && tc.q.GetSources() > 0 && !strings.Contains(got.Detail, "orch quarantine list"):
			t.Fatalf("detail = %q", got.Detail)
		}
	}
}
