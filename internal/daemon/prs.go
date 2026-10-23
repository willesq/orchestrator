package daemon

import (
	"context"
	"os"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// ListPRs serves the PR babysitter's cache. It reads the file the babysitter
// writes and never calls gh, so it is cheap enough to poll.
func (s *server) ListPRs(_ context.Context, req *orchdv1.ListPRsRequest) (*orchdv1.ListPRsResponse, error) {
	state, err := prs.ReadState(s.prStateFile)
	if err != nil {
		// Unavailable, not an empty list: the app keeps its last good list
		// instead of treating every PR as new once the file reads again.
		return nil, grpcstatus.Errorf(codes.Unavailable, "PR cache unreadable: %v", err)
	}
	list := prs.Ordered(state)
	if req.GetNeedsMe() {
		list = prs.NeedsMe(state)
	}
	out := &orchdv1.ListPRsResponse{Prs: make([]*orchdv1.PullRequest, 0, len(list))}
	if fi, err := os.Stat(s.prStateFile); err == nil {
		out.UpdatedAt = fi.ModTime().Unix()
	}
	for _, pr := range list {
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
