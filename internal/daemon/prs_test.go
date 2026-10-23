package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestListPRs(t *testing.T) {
	cache := prs.State{PRs: map[string]prs.PR{
		"https://x/quiet": {URL: "https://x/quiet", Title: "quiet", Repo: "a/a",
			Reasons: []string{prs.ReasonAuthored}, CIState: "pending"},
		"https://x/review": {URL: "https://x/review", Title: "review", Repo: "b/b",
			Reasons: []string{prs.ReasonReviewRequested}, Mergeable: "MERGEABLE"},
		"https://x/broken": {URL: "https://x/broken", Title: "broken", Repo: "c/c",
			Reasons: []string{prs.ReasonAuthored}, CIState: "failure", ReviewDecision: "APPROVED", IsDraft: false},
	}}
	tests := []struct {
		name     string
		seed     bool
		needsMe  bool
		wantURLs []string
	}{
		{"all PRs, attention first", true, false, []string{"https://x/broken", "https://x/review", "https://x/quiet"}},
		{"only PRs needing me", true, true, []string{"https://x/broken", "https://x/review"}},
		{"no cache yet", false, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pr_state.json")
			if tt.seed {
				if err := prs.SaveState(path, cache); err != nil {
					t.Fatal(err)
				}
			}
			s := &server{prStateFile: path}
			resp, err := s.ListPRs(context.Background(), &orchdv1.ListPRsRequest{NeedsMe: tt.needsMe})
			if err != nil {
				t.Fatalf("ListPRs: %v", err)
			}
			var urls []string
			for _, pr := range resp.GetPrs() {
				urls = append(urls, pr.GetUrl())
			}
			if !slices.Equal(urls, tt.wantURLs) {
				t.Errorf("urls = %v, want %v", urls, tt.wantURLs)
			}
			if tt.seed != (resp.GetUpdatedAt() > 0) {
				t.Errorf("updated_at = %d with seed=%v", resp.GetUpdatedAt(), tt.seed)
			}
		})
	}
}

func TestListPRsMapsEveryField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr_state.json")
	pr := prs.PR{URL: "https://x/1", Title: "fix", Repo: "o/r", CIState: "failure",
		ReviewDecision: "CHANGES_REQUESTED", Mergeable: "CONFLICTING", IsDraft: false,
		Reasons: []string{prs.ReasonAuthored, prs.ReasonReviewRequested}}
	if err := prs.SaveState(path, prs.State{PRs: map[string]prs.PR{pr.URL: pr}}); err != nil {
		t.Fatal(err)
	}
	resp, err := (&server{prStateFile: path}).ListPRs(context.Background(), &orchdv1.ListPRsRequest{})
	if err != nil || len(resp.GetPrs()) != 1 {
		t.Fatalf("ListPRs = %v, %v", resp, err)
	}
	got := resp.GetPrs()[0]
	if got.GetUrl() != pr.URL || got.GetTitle() != pr.Title || got.GetRepo() != pr.Repo ||
		got.GetCiState() != pr.CIState || got.GetReviewDecision() != pr.ReviewDecision ||
		got.GetMergeable() != pr.Mergeable || got.GetIsDraft() != pr.IsDraft ||
		!slices.Equal(got.GetReasons(), pr.Reasons) || got.GetAttention() != prs.AttentionCIFailed {
		t.Errorf("mapped PR = %+v", got)
	}
}

func TestListPRsRefusesACorruptCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr_state.json")
	if err := os.WriteFile(path, []byte(`{"prs": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&server{prStateFile: path}).ListPRs(context.Background(), &orchdv1.ListPRsRequest{})
	if grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable rather than an empty list", err)
	}
}
