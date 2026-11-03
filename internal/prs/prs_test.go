package prs

import (
	"path/filepath"
	"testing"
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

func TestCountChecks(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   string
	}{
		{"none", nil, "no checks"},
		{"mixed", []Check{{Conclusion: "SUCCESS"}, {State: "SUCCESS"}, {Conclusion: "SKIPPED"}, {Conclusion: "NEUTRAL"}, {Conclusion: "FAILURE"}},
			"2 passed, 2 skipped, 1 failed"},
		{"all skipped", []Check{{Conclusion: "SKIPPED"}}, "0 passed, 1 skipped, 0 failed"},
		{"running is named only when there is some", []Check{{Conclusion: "SUCCESS"}, {Status: "IN_PROGRESS"}, {State: "PENDING"}},
			"1 passed, 0 skipped, 0 failed, 2 running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountChecks(tt.checks).String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
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
		{name: "queued check run is pending", checks: []chk{{Status: "QUEUED"}}, want: "pending"},
		{name: "commit status pending", checks: []chk{{State: "PENDING"}}, want: "pending"},
		{name: "skipped passes", checks: []chk{{Conclusion: "SUCCESS"}, {Status: "COMPLETED", Conclusion: "SKIPPED"}}, want: "success"},
		{name: "only skipped passes", checks: []chk{{Conclusion: "SKIPPED"}}, want: "success"},
		{name: "neutral passes", checks: []chk{{Conclusion: "NEUTRAL"}}, want: "success"},
		{name: "skipped does not hide a running check", checks: []chk{{Conclusion: "SKIPPED"}, {Status: "IN_PROGRESS"}}, want: "pending"},
		{name: "skipped does not hide a failure", checks: []chk{{Conclusion: "SKIPPED"}, {Conclusion: "FAILURE"}}, want: "failure"},
		{name: "action required fails", checks: []chk{{Conclusion: "ACTION_REQUIRED"}}, want: "failure"},
		{name: "startup failure fails", checks: []chk{{Conclusion: "STARTUP_FAILURE"}}, want: "failure"},
		{name: "stale fails", checks: []chk{{Conclusion: "STALE"}}, want: "failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RollupCI(tt.checks); got != tt.want {
				t.Errorf("RollupCI = %q, want %q", got, tt.want)
			}
		})
	}
}
