package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
)

func TestPRsShowsCheckCounts(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	_ = prs.SaveState(paths.PRStateFile(), prs.State{PRs: map[string]prs.PR{
		"https://x/a": {URL: "https://x/a", Title: "a", Repo: "o/r", Reasons: []string{prs.ReasonAuthored}, CIState: "success", Checks: "0 passed, 1 skipped, 0 failed"},
		"https://x/b": {URL: "https://x/b", Title: "b", Repo: "o/r", Reasons: []string{prs.ReasonAuthored}, CIState: "none"},
	}})
	out, err := run(factory(&fakeClient{}, nil), "", "prs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "checks:") != 1 || !strings.Contains(out, "checks: 0 passed, 1 skipped, 0 failed") {
		t.Errorf("out = %q", out)
	}
}

func TestPRsNeedsMe(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub []string
		wantNot []string
		wantErr string
	}{
		{
			name:    "lists only PRs needing me, most urgent first",
			args:    []string{"prs", "--needs-me"},
			wantSub: []string{"needs you: ci_failed", "needs you: review_requested"},
			wantNot: []string{"o/r: quiet"},
		},
		{
			name:    "cannot combine with --blocked",
			args:    []string{"prs", "--needs-me", "--blocked"},
			wantErr: "none of the others can be",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			_ = prs.SaveState(paths.PRStateFile(), prs.State{PRs: map[string]prs.PR{
				"https://x/review": {URL: "https://x/review", Title: "review", Repo: "o/r", Reasons: []string{prs.ReasonReviewRequested}},
				"https://x/broken": {URL: "https://x/broken", Title: "broken", Repo: "o/r", Reasons: []string{prs.ReasonAuthored}, CIState: "failure"},
				"https://x/quiet":  {URL: "https://x/quiet", Title: "quiet", Repo: "o/r", Reasons: []string{prs.ReasonAuthored}, CIState: "pending"},
			}})
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("prs: %v", err)
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(out, s) {
					t.Errorf("output missing %q:\n%s", s, out)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(out, s) {
					t.Errorf("output should not contain %q:\n%s", s, out)
				}
			}
			if strings.Index(out, "ci_failed") > strings.Index(out, "review_requested") {
				t.Errorf("failing CI should come before a review request:\n%s", out)
			}
		})
	}
}
