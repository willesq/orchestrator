package status

import (
	"context"
	"errors"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesSection covers SP-020.2.a3's "visible in orch status": drift from
// a pending draft or an edit outside orchd shows as its own line.
func TestSpacesSection(t *testing.T) {
	tests := []struct {
		name  string
		resp  *orchdv1.StatusResponse
		err   error
		lines []string // "name=value/level"; nil: no spaces section
	}{
		{name: "daemon down", err: errors.New("nope")},
		{name: "older daemon", resp: &orchdv1.StatusResponse{}},
		{name: "spaces never used", resp: &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{}}},
		{
			name:  "draft saved, nothing applied",
			resp:  &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{DraftRevision: 1, Drift: []string{"draft"}}},
			lines: []string{"policy=none applied/ok", "drift=draft revision 1 not applied/warn"},
		},
		{
			name:  "applied but empty",
			resp:  &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{AppliedRevision: 2}},
			lines: []string{"policy=revision 2 applied/ok"},
		},
		{
			name: "pending draft",
			resp: &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{
				Configured: true, AppliedRevision: 7, DraftRevision: 8, Drift: []string{"draft"},
			}},
			lines: []string{"policy=revision 7 applied/ok", "drift=draft revision 8 not applied/warn"},
		},
		{
			name: "changed outside orchd, invalid draft",
			resp: &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{
				Configured: true, AppliedRevision: 7, DraftRevision: 8,
				Drift: []string{"draft", "changed_outside_orchd", "future"},
				Error: "draft revision 8 does not validate",
			}},
			lines: []string{
				"policy=revision 7 applied/ok",
				"drift=draft revision 8 not applied/warn",
				"drift=applied revision changed outside orchd/bad",
				"drift=future/warn",
				"error=draft revision 8 does not validate/bad",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) { return tt.resp, tt.err }
			r := Collect(context.Background(), d)
			if tt.lines == nil {
				for _, s := range r.Sections {
					if s.Title == "spaces" {
						t.Fatalf("spaces section shown: %+v", s)
					}
				}
				return
			}
			if r.Sections[2].Title != "spaces" {
				t.Fatalf("section 2 is %q, want spaces", r.Sections[2].Title)
			}
			sec := section(t, r, "spaces")
			var got []string
			for _, l := range sec.Lines {
				got = append(got, l.Name+"="+l.Value+"/"+l.Level)
			}
			if strings.Join(got, "\n") != strings.Join(tt.lines, "\n") {
				t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.lines, "\n"))
			}
		})
	}
}
