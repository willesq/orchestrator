package daemon

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/evidence"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// taskRater records verdicts on finished tasks. It is kept apart from the
// read-only taskReader so the ledger's only writer besides the queue is this one
// narrow call.
type taskRater interface {
	Rate(ctx context.Context, id, rating string) error
}

type dbRater struct{ db *sql.DB }

func (r dbRater) Rate(ctx context.Context, id, rating string) error {
	return evidence.Rate(ctx, r.db, id, rating, time.Now().Unix())
}

func (s *server) RateTask(ctx context.Context, req *orchdv1.RateTaskRequest) (*orchdv1.RateTaskResponse, error) {
	if !evidence.ValidOutcome(req.GetRating()) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "rating must be good, rework or bad")
	}
	if s.rater == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task ratings are unavailable")
	}
	err := s.rater.Rate(ctx, req.GetId(), req.GetRating())
	if errors.Is(err, evidence.ErrNoTask) {
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	s.hub.publish(req.GetId())
	return &orchdv1.RateTaskResponse{}, nil
}
