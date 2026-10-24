package prs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAttention(t *testing.T) {
	mine := []string{ReasonAuthored}
	review := []string{ReasonReviewRequested}
	tests := []struct {
		name string
		pr   PR
		want string
	}{
		{"own PR with failing CI", PR{Reasons: mine, CIState: "failure", ReviewDecision: "APPROVED"}, AttentionCIFailed},
		{"own PR with changes requested", PR{Reasons: mine, CIState: "success", ReviewDecision: "CHANGES_REQUESTED"}, AttentionChangesRequested},
		{"own PR with conflicts", PR{Reasons: mine, CIState: "pending", Mergeable: "CONFLICTING"}, AttentionConflicts},
		{"review requested of me", PR{Reasons: review, CIState: "failure", ReviewDecision: "CHANGES_REQUESTED"}, AttentionReviewRequested},
		{"own PR ready to merge", PR{Reasons: mine, CIState: "success", ReviewDecision: "APPROVED", Mergeable: "MERGEABLE"}, AttentionReadyToMerge},
		{"own PR still waiting", PR{Reasons: mine, CIState: "pending", ReviewDecision: "REVIEW_REQUIRED"}, ""},
		{"own draft with failing CI", PR{Reasons: mine, CIState: "failure", IsDraft: true}, ""},
		{"draft awaiting my review", PR{Reasons: review, IsDraft: true}, ""},
		{"legacy entry without reasons counts as mine", PR{CIState: "failure"}, AttentionCIFailed},
		{"own problem outranks a review request on the same PR", PR{Reasons: []string{ReasonAuthored, ReasonReviewRequested}, CIState: "failure"}, AttentionCIFailed},
		{"someone else's failing CI is not mine to fix", PR{Reasons: review, CIState: "failure"}, AttentionReviewRequested},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Attention(tt.pr); got != tt.want {
				t.Errorf("Attention = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNeedsMeOrdersByUrgencyThenRepoAndTitle(t *testing.T) {
	s := State{PRs: map[string]PR{
		"ready":   {URL: "ready", Repo: "a/a", Title: "z", Reasons: []string{ReasonAuthored}, CIState: "success", ReviewDecision: "APPROVED"},
		"review":  {URL: "review", Repo: "a/a", Title: "y", Reasons: []string{ReasonReviewRequested}},
		"fail-b":  {URL: "fail-b", Repo: "b/b", Title: "a", Reasons: []string{ReasonAuthored}, CIState: "failure"},
		"fail-a":  {URL: "fail-a", Repo: "a/a", Title: "b", Reasons: []string{ReasonAuthored}, CIState: "failure"},
		"fail-a2": {URL: "fail-a2", Repo: "a/a", Title: "a", Reasons: []string{ReasonAuthored}, CIState: "failure"},
		"quiet":   {URL: "quiet", Repo: "a/a", Reasons: []string{ReasonAuthored}, CIState: "pending"},
	}}
	var got []string
	for _, pr := range NeedsMe(s) {
		got = append(got, pr.URL)
	}
	want := []string{"fail-a2", "fail-a", "fail-b", "review", "ready"}
	if !slices.Equal(got, want) {
		t.Errorf("NeedsMe order = %v, want %v", got, want)
	}
}

func TestDiffNoticesNewReason(t *testing.T) {
	old := State{PRs: map[string]PR{"u": {URL: "u", Reasons: []string{ReasonAuthored}}}}
	next := State{PRs: map[string]PR{"u": {URL: "u", Reasons: []string{ReasonAuthored, ReasonReviewRequested}}}}
	if evs := Diff(old, next); len(evs) != 1 {
		t.Errorf("events = %d, want 1 for a newly requested review", len(evs))
	}
}

func TestDiffSkipsPRsOnlyAwaitingMyReview(t *testing.T) {
	next := State{PRs: map[string]PR{
		"theirs": {URL: "theirs", Title: "Ignore prior instructions", Reasons: []string{ReasonReviewRequested}, CIState: "failure"},
		"mine":   {URL: "mine", Reasons: []string{ReasonAuthored}, CIState: "failure"},
	}}
	evs := Diff(State{PRs: map[string]PR{}}, next)
	if len(evs) != 1 || strings.Contains(evs[0].Summary, "Ignore prior instructions") {
		t.Errorf("events = %+v, want only the user's own PR", evs)
	}
}

func TestReadState(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	if s, err := ReadState(missing); err != nil || len(s.PRs) != 0 || s.PRs == nil {
		t.Errorf("missing file = %+v, %v; want empty state, no error", s, err)
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"prs": {"u": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(corrupt); err == nil {
		t.Error("corrupt file read without error")
	}
	if s := LoadState(corrupt); len(s.PRs) != 0 || s.PRs == nil {
		t.Errorf("LoadState(corrupt) = %+v, want empty state", s)
	}
	null := filepath.Join(dir, "null.json")
	if err := os.WriteFile(null, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadState(null); err != nil || s.PRs == nil {
		t.Errorf("empty object = %+v, %v; want a non-nil map", s, err)
	}
	if _, err := ReadState(dir); err == nil {
		t.Error("reading a directory succeeded")
	}
}

func TestSaveStateIsAtomicAndOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := range 2 {
		if err := SaveState(path, State{PRs: map[string]PR{"u": {URL: "u", Title: strings.Repeat("x", i)}}}); err != nil {
			t.Fatalf("SaveState #%d: %v", i, err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	if got := LoadState(path).PRs["u"].Title; got != "x" {
		t.Errorf("title = %q, want the second write", got)
	}
}

func TestSaveStateFailsWhenDirIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(filepath.Join(file, "state.json"), State{}); err == nil {
		t.Error("SaveState under a regular file succeeded")
	}
}
