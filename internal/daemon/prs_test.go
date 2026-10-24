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

func TestListGitHubItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr_state.json")
	cache := prs.State{PRs: map[string]prs.PR{
		"https://x/pr":    {URL: "https://x/pr", Title: "fix", Repo: "WippData/api", Kind: prs.KindPR, Number: 3, Author: "willesq", Reasons: []string{prs.ReasonAuthored}, CIState: "failure"},
		"https://x/issue": {URL: "https://x/issue", Title: "bug", Repo: "willesq/orch", Kind: prs.KindIssue, Number: 9, Reasons: []string{prs.ReasonAssigned}, UpdatedAt: "2026-10-23T10:00:00Z"},
		"https://x/old":   {URL: "https://x/old", Title: "legacy", Repo: "willesq/orch"},
	}}
	if err := prs.SaveState(path, cache); err != nil {
		t.Fatal(err)
	}
	s := &server{prStateFile: path}
	ctx := context.Background()
	tests := []struct {
		name string
		req  *orchdv1.ListGitHubItemsRequest
		want []string
	}{
		{"everything, PR needing the user first", &orchdv1.ListGitHubItemsRequest{}, []string{"https://x/pr", "https://x/issue", "https://x/old"}},
		{"issues only", &orchdv1.ListGitHubItemsRequest{Kind: "issue"}, []string{"https://x/issue"}},
		{"PRs only, legacy entries count as PRs", &orchdv1.ListGitHubItemsRequest{Kind: "pr"}, []string{"https://x/pr", "https://x/old"}},
		{"one owner, case-insensitive", &orchdv1.ListGitHubItemsRequest{Owner: "wippdata"}, []string{"https://x/pr"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := s.ListGitHubItems(ctx, tt.req)
			if err != nil {
				t.Fatal(err)
			}
			var urls []string
			for _, it := range resp.GetItems() {
				urls = append(urls, it.GetUrl())
			}
			if !slices.Equal(urls, tt.want) || resp.GetUpdatedAt() == 0 {
				t.Errorf("urls = %v (updated_at %d), want %v", urls, resp.GetUpdatedAt(), tt.want)
			}
		})
	}
	resp, _ := s.ListGitHubItems(ctx, &orchdv1.ListGitHubItemsRequest{})
	first := resp.GetItems()[0]
	if first.GetKind() != "pr" || first.GetNumber() != 3 || first.GetAuthor() != "willesq" || first.GetAttention() != prs.AttentionCIFailed {
		t.Errorf("mapped item = %+v", first)
	}
	if issue := resp.GetItems()[1]; issue.GetAttention() != "" || issue.GetUpdatedAt() != "2026-10-23T10:00:00Z" {
		t.Errorf("issue = %+v", issue)
	}
	if _, err := s.ListGitHubItems(ctx, &orchdv1.ListGitHubItemsRequest{Kind: "discussion"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad kind: %v", err)
	}
	pr, _ := s.ListPRs(ctx, &orchdv1.ListPRsRequest{})
	if len(pr.GetPrs()) != 2 {
		t.Errorf("ListPRs returned %d items, want the 2 PRs", len(pr.GetPrs()))
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListGitHubItems(ctx, &orchdv1.ListGitHubItemsRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("corrupt cache: %v", err)
	}
}
