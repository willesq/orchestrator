package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestSpacesSectionNamesRelaxedLayers is the orch status half of
// SP-070.2.a1: while a layer is off, orch status names it and since when,
// whether or not spaces are configured.
func TestSpacesSectionNamesRelaxedLayers(t *testing.T) {
	at := time.Date(2026, 11, 11, 10, 5, 0, 0, time.Local)
	resp := &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Relaxed: []*orchdv1.EnforcementSwitch{
		{Layer: "guard", Title: "tool guard", State: "off", DefaultState: "block-other-jobs", Relaxed: true,
			SetAt: at.Unix(), SetBy: "operator (process 9)", Until: at.Add(time.Hour).Unix(), RevertsTo: "block-other-jobs"},
		{Layer: "memory", State: "off", DefaultState: "on", Relaxed: true},
	}}}
	d := healthyDeps(t)
	d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) { return resp, nil }
	sec := section(t, Collect(context.Background(), d), "spaces")
	if len(sec.Lines) != 3 {
		t.Fatalf("lines = %+v", sec.Lines)
	}
	g, m := sec.Lines[1], sec.Lines[2]
	if g.Name != "enforcement" || g.Value != "guard is off" || g.Level != LevelWarn {
		t.Errorf("guard line = %+v", g)
	}
	for _, want := range []string{"tool guard", "since 2026-11-11 10:05", "reverts to block-other-jobs at 2026-11-11 11:05",
		"set by operator (process 9)", "orch spaces enforce guard block-other-jobs"} {
		if !strings.Contains(g.Detail, want) {
			t.Errorf("guard detail %q lacks %q", g.Detail, want)
		}
	}
	if m.Value != "memory is off" || strings.Contains(m.Detail, "since") || !strings.Contains(m.Detail, "orch spaces enforce memory on") {
		t.Errorf("memory line = %+v", m)
	}

	// A layer not enforcing though its switch is on, and switches orch.db
	// could not give orchd, without spaces configured.
	resp = &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{
		EnforcementError: "the enforcement switches in orch.db were changed outside orchd\x1b[2J",
		Relaxed: []*orchdv1.EnforcementSwitch{{Layer: "hooks", State: "on", DefaultState: "on", Relaxed: true,
			Problem: "PreToolUse is removed", SetBy: "operator\x1b]0;evil\a"}},
	}}
	sec = section(t, Collect(context.Background(), d), "spaces")
	if len(sec.Lines) != 3 || sec.Lines[1].Value != "hooks does not enforce" || !strings.Contains(sec.Lines[1].Detail, "PreToolUse is removed") ||
		sec.Lines[2].Value != "switches in orch.db not taken" || sec.Lines[2].Level != LevelBad {
		t.Fatalf("lines = %+v", sec.Lines)
	}
	for _, l := range sec.Lines {
		if strings.ContainsAny(l.Detail+l.Value, "\x1b\a") {
			t.Errorf("control characters reach the terminal: %q", l.Detail)
		}
	}
}
