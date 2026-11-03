package prs

import (
	"encoding/json"
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

func TestLatestChecks(t *testing.T) {
	run := func(name, started, conclusion string) Check {
		return Check{WorkflowName: "ci", Name: name, StartedAt: started, Status: "COMPLETED", Conclusion: conclusion}
	}
	const t0, t1, t2 = "2026-11-01T00:00:00Z", "2026-11-01T00:00:41Z", "2026-11-06T00:00:00Z"
	queued := Check{WorkflowName: "ci", Name: "title", StartedAt: "0001-01-01T00:00:00Z", Status: "QUEUED"}
	tests := []struct {
		name   string
		checks []Check
		ci     string
		counts string
	}{
		{"a superseded failure does not count", []Check{run("title", t0, "FAILURE"), run("title", t1, "SUCCESS"), run("title", t2, "SUCCESS")},
			"success", "1 passed, 0 skipped, 0 failed"},
		{"the order gh returns them in does not matter", []Check{run("title", t2, "SUCCESS"), run("title", t0, "FAILURE")},
			"success", "1 passed, 0 skipped, 0 failed"},
		{"a superseded cancel does not count", []Check{run("build", t0, "CANCELLED"), run("build", t1, "SUCCESS"), run("lint", t0, "SKIPPED")},
			"success", "1 passed, 1 skipped, 0 failed"},
		{"a newer failure does", []Check{run("title", t0, "SUCCESS"), run("title", t1, "FAILURE")},
			"failure", "0 passed, 0 skipped, 1 failed"},
		{"a re-run still queued is the newest", []Check{run("title", t1, "SUCCESS"), queued},
			"pending", "0 passed, 0 skipped, 0 failed, 1 running"},
		{"a re-run with no start time at all is the newest", []Check{run("title", t1, "FAILURE"), {WorkflowName: "ci", Name: "title", Status: "QUEUED"}},
			"pending", "0 passed, 0 skipped, 0 failed, 1 running"},
		{"a finished run with no start time is not", []Check{run("title", "0001-01-01T00:00:00Z", "CANCELLED"), run("title", t0, "SUCCESS")},
			"success", "1 passed, 0 skipped, 0 failed"},
		{"a later skip does not erase a failure", []Check{run("title", t0, "FAILURE"), run("title", t1, "SKIPPED")},
			"failure", "0 passed, 0 skipped, 1 failed"},
		{"nor does the order they arrive in matter", []Check{run("title", t1, "NEUTRAL"), run("title", t0, "FAILURE")},
			"failure", "0 passed, 0 skipped, 1 failed"},
		{"a skip after a later pass counts as the skip", []Check{run("title", t0, "FAILURE"), run("title", t1, "SUCCESS"), run("title", t2, "SKIPPED")},
			"success", "0 passed, 1 skipped, 0 failed"},
		{"a tie keeps the worse result", []Check{run("title", t0, "SUCCESS"), run("title", t0, "FAILURE"), run("build", t0, "FAILURE"), run("build", t0, "SUCCESS")},
			"failure", "0 passed, 0 skipped, 2 failed"},
		{"the same job name in another workflow is another check", []Check{run("test", t0, "FAILURE"), {WorkflowName: "nightly", Name: "test", StartedAt: t1, Conclusion: "SUCCESS"}},
			"failure", "1 passed, 0 skipped, 1 failed"},
		{"commit statuses go by context", []Check{{Context: "ci/x", StartedAt: t0, State: "FAILURE"}, {Context: "ci/x", StartedAt: t1, State: "SUCCESS"}, {Context: "ci/y", State: "SUCCESS"}},
			"success", "2 passed, 0 skipped, 0 failed"},
		{"entries with no identity are all kept", []Check{{Conclusion: "FAILURE"}, {Conclusion: "SUCCESS"}},
			"failure", "1 passed, 0 skipped, 1 failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RollupCI(tt.checks); got != tt.ci {
				t.Errorf("RollupCI = %q, want %q", got, tt.ci)
			}
			if got := CountChecks(tt.checks).String(); got != tt.counts {
				t.Errorf("CountChecks = %q, want %q", got, tt.counts)
			}
		})
	}
}

func TestReplacedFailures(t *testing.T) {
	run := func(started, conclusion string) Check {
		return Check{WorkflowName: "ci", Name: "title", StartedAt: started, Conclusion: conclusion}
	}
	tests := []struct {
		name   string
		checks []Check
		want   int
	}{
		{"none", nil, 0},
		{"replaced by a pass", []Check{run("2026-11-01T00:00:00Z", "FAILURE"), run("2026-11-01T00:01:00Z", "SUCCESS")}, 1},
		{"two replaced", []Check{run("2026-11-01T00:00:00Z", "FAILURE"), run("2026-11-01T00:00:30Z", "CANCELLED"), run("2026-11-01T00:01:00Z", "SUCCESS")}, 2},
		{"the failure still stands", []Check{run("2026-11-01T00:00:00Z", "SUCCESS"), run("2026-11-01T00:01:00Z", "FAILURE")}, 0},
		{"kept over a skip", []Check{run("2026-11-01T00:00:00Z", "FAILURE"), run("2026-11-01T00:01:00Z", "SKIPPED")}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReplacedFailures(tt.checks); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestChecksDecodeFromGH(t *testing.T) {
	var got []Check
	raw := `[{"__typename":"CheckRun","name":"title","workflowName":"ci","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-01T00:00:00Z"},
		{"__typename":"CheckRun","name":"title","workflowName":"ci","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T00:00:41Z"},
		{"__typename":"StatusContext","context":"ci/x","state":"SUCCESS","startedAt":"2026-11-01T00:00:00Z"}]`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if ci := RollupCI(got); ci != "success" {
		t.Errorf("RollupCI = %q", ci)
	}
	if n := len(LatestChecks(got)); n != 2 {
		t.Errorf("LatestChecks kept %d", n)
	}
}

func TestRollupCI(t *testing.T) {
	type chk = Check
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
		{name: "stale is pending", checks: []chk{{Conclusion: "STALE"}}, want: "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RollupCI(tt.checks); got != tt.want {
				t.Errorf("RollupCI = %q, want %q", got, tt.want)
			}
		})
	}
}
