package daemon

import (
	"context"
	"math"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// ListPRs serves the PR babysitter's cache. It reads the file the babysitter
// writes and never calls gh, so it is cheap enough to poll.
func (s *server) ListPRs(ctx context.Context, req *orchdv1.ListPRsRequest) (*orchdv1.ListPRsResponse, error) {
	state, err := prs.ReadState(s.prStateFile)
	if err != nil {
		// Unavailable, not an empty list: the app keeps its last good list
		// instead of treating every PR as new once the file reads again.
		return nil, grpcstatus.Errorf(codes.Unavailable, "PR cache unreadable: %v", err)
	}
	list := prs.OnlyPRs(prs.Ordered(state))
	if req.GetNeedsMe() {
		list = prs.NeedsMe(state)
	}
	out := &orchdv1.ListPRsResponse{Prs: make([]*orchdv1.PullRequest, 0, len(list))}
	if fi, err := os.Stat(s.prStateFile); err == nil {
		out.UpdatedAt = fi.ModTime().Unix()
	}
	hidden := s.profileItemsHidden(ctx)
	for _, pr := range list {
		if hidden(pr.Repo) {
			continue
		}
		out.Prs = append(out.Prs, &orchdv1.PullRequest{
			Url:            pr.URL,
			Title:          pr.Title,
			Repo:           pr.Repo,
			CiState:        pr.CIState,
			ReviewDecision: pr.ReviewDecision,
			Mergeable:      pr.Mergeable,
			IsDraft:        pr.IsDraft,
			Reasons:        pr.Reasons,
			Attention:      prs.Attention(pr),
		})
	}
	return out, nil
}

// profileItemsHidden returns whether an item of repo (owner/name) is left
// out for this caller: an agent does not see the items of an owner bound to
// a space with its own gh profile, which only that job's account can read
// (SP-074). The operator sees every item.
func (s *server) profileItemsHidden(ctx context.Context) func(repo string) bool {
	if _, op := operator(ctx); op || s.ghProfiles == nil {
		return func(string) bool { return false }
	}
	seen := map[string]bool{}
	return func(repo string) bool {
		owner, _, _ := strings.Cut(repo, "/")
		key := strings.ToLower(owner)
		hide, ok := seen[key]
		if !ok {
			hide = s.ghProfiles.ownsProfile(owner)
			seen[key] = hide
		}
		return hide
	}
}

// ListGitHubItems serves the watcher's cache of PRs and issues, optionally for
// one kind or owner. Like ListPRs it never calls gh.
func (s *server) ListGitHubItems(ctx context.Context, req *orchdv1.ListGitHubItemsRequest) (*orchdv1.ListGitHubItemsResponse, error) {
	if k := req.GetKind(); k != "" && k != prs.KindPR && k != prs.KindIssue {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "kind must be %q or %q", prs.KindPR, prs.KindIssue)
	}
	state, err := prs.ReadState(s.prStateFile)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "PR cache unreadable: %v", err)
	}
	out := &orchdv1.ListGitHubItemsResponse{}
	if fi, err := os.Stat(s.prStateFile); err == nil {
		out.UpdatedAt = fi.ModTime().Unix()
	}
	hidden := s.profileItemsHidden(ctx)
	for _, item := range prs.Ordered(state) {
		if hidden(item.Repo) {
			continue
		}
		kind := item.Kind
		if kind == "" {
			kind = prs.KindPR
		}
		owner, _, _ := strings.Cut(item.Repo, "/")
		if req.GetKind() != "" && kind != req.GetKind() || req.GetOwner() != "" && !strings.EqualFold(owner, req.GetOwner()) {
			continue
		}
		out.Items = append(out.Items, &orchdv1.GitHubItem{
			Url: item.URL, Title: item.Title, Repo: item.Repo, Kind: kind, Number: int32(min(item.Number, math.MaxInt32)),
			Author: item.Author, UpdatedAt: item.UpdatedAt, IsDraft: item.IsDraft, CiState: item.CIState,
			ReviewDecision: item.ReviewDecision, Mergeable: item.Mergeable, Reasons: item.Reasons,
			Attention: prs.Attention(item),
		})
	}
	return out, nil
}
