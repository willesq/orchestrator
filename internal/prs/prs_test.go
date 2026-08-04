package prs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/events"
)

func TestLoadSaveState(t *testing.T) {
	tests := []struct {
		name  string
		write bool
		want  int
	}{
		{name: "missing file yields empty", write: false, want: 0},
		{name: "round-trips saved state", write: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if tt.write {
				if err := SaveState(path, State{PRs: map[string]PR{"u": {URL: "u", Repo: "o/r"}}}); err != nil {
					t.Fatalf("SaveState: %v", err)
				}
			}
			s := LoadState(path)
			if len(s.PRs) != tt.want {
				t.Errorf("PRs = %d, want %d", len(s.PRs), tt.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name    string
		old     State
		next    State
		wantEvs int
	}{
		{
			name:    "new PR emits event",
			old:     State{PRs: map[string]PR{}},
			next:    State{PRs: map[string]PR{"u": {URL: "u", CIState: "pending"}}},
			wantEvs: 1,
		},
		{
			name:    "unchanged emits nothing",
			old:     State{PRs: map[string]PR{"u": {URL: "u", CIState: "success", ReviewDecision: "APPROVED"}}},
			next:    State{PRs: map[string]PR{"u": {URL: "u", CIState: "success", ReviewDecision: "APPROVED"}}},
			wantEvs: 0,
		},
		{
			name:    "ci transition emits event",
			old:     State{PRs: map[string]PR{"u": {URL: "u", CIState: "pending"}}},
			next:    State{PRs: map[string]PR{"u": {URL: "u", CIState: "failure"}}},
			wantEvs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Diff(tt.old, tt.next); len(got) != tt.wantEvs {
				t.Errorf("events = %d, want %d", len(got), tt.wantEvs)
			}
		})
	}
}

func TestClassifyAndFilter(t *testing.T) {
	tests := []struct {
		name     string
		pr       PR
		wantKind string
		blocked  bool
		ready    bool
	}{
		{name: "failing is blocked", pr: PR{CIState: "failure"}, wantKind: "blocked", blocked: true},
		{name: "changes requested is blocked", pr: PR{ReviewDecision: "CHANGES_REQUESTED"}, wantKind: "blocked", blocked: true},
		{name: "green+approved is complete/ready", pr: PR{CIState: "success", ReviewDecision: "APPROVED"}, wantKind: "complete", ready: true},
		{name: "pending is info", pr: PR{CIState: "pending"}, wantKind: "info"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.pr); got != tt.wantKind {
				t.Errorf("classify = %q, want %q", got, tt.wantKind)
			}
			state := State{PRs: map[string]PR{"u": tt.pr}}
			if got := Filter(state, true, false); (len(got) == 1) != tt.blocked {
				t.Errorf("blocked filter = %v, want %v", len(got) == 1, tt.blocked)
			}
			if got := Filter(state, false, true); (len(got) == 1) != tt.ready {
				t.Errorf("ready filter = %v, want %v", len(got) == 1, tt.ready)
			}
		})
	}
}

func TestRollupCI(t *testing.T) {
	type chk = struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	}
	tests := []struct {
		name   string
		checks []chk
		want   string
	}{
		{name: "empty is none", checks: nil, want: "none"},
		{name: "any failure", checks: []chk{{Conclusion: "SUCCESS"}, {Conclusion: "FAILURE"}}, want: "failure"},
		{name: "all success", checks: []chk{{Conclusion: "SUCCESS"}}, want: "success"},
		{name: "in progress is pending", checks: []chk{{State: "IN_PROGRESS"}}, want: "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollupCI(tt.checks); got != tt.want {
				t.Errorf("rollupCI = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSweep(t *testing.T) {
	tests := []struct {
		name       string
		wantEvents int
	}{
		{name: "sweep polls, saves state, emits events", wantEvents: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stateFile := filepath.Join(dir, "state.json")
			eventsDir := filepath.Join(dir, "events")
			b := New(stateFile, eventsDir)
			// Fake gh: phase 1 discovery, phase 2 enrichment.
			b.gh = func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "search" {
					return json.Marshal([]map[string]any{
						{"url": "https://x/1", "title": "fix", "isDraft": false,
							"repository": map[string]any{"nameWithOwner": "o/r"}},
					})
				}
				return json.Marshal(map[string]any{
					"statusCheckRollup": []map[string]any{{"conclusion": "FAILURE"}},
					"reviewDecision":    "",
					"mergeable":         "UNKNOWN",
				})
			}
			if err := b.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			state := LoadState(stateFile)
			if len(state.PRs) != 1 {
				t.Errorf("state PRs = %d, want 1", len(state.PRs))
			}
			if state.PRs["https://x/1"].CIState != "failure" {
				t.Errorf("ci not enriched: %+v", state.PRs["https://x/1"])
			}
			evs, _ := events.Read(eventsDir, "pr-babysitter")
			if len(evs) != tt.wantEvents {
				t.Errorf("events = %d, want %d", len(evs), tt.wantEvents)
			}
		})
	}
}
