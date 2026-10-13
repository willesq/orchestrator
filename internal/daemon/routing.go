package daemon

import (
	"context"
	"database/sql"
	"errors"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/routing"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/workflows"
)

// modelRouter is the routing the RPC surface needs.
type modelRouter interface {
	Recommend(ctx context.Context, kind string, runnable func(string) bool) (routing.Recommendation, error)
	Pin(ctx context.Context, ref string, runnable func(string) bool) (routing.Recommendation, error)
}

// workerRunnable reports whether the queue worker can execute a provider's
// models. It runs Claude Code only, so routes elsewhere fall to the next rank.
func workerRunnable(provider string) bool { return provider == models.ProviderClaude }

// route resolves a dispatch's model. An explicit model wins over a kind; with
// neither the task is unrouted and runs on the agent's own model. A kind with
// no routable model is recorded but runs unrouted rather than failing, since
// the agent's default is still a working choice.
func (s *server) route(ctx context.Context, req *orchdv1.DispatchRequest) (workflows.Route, *orchdv1.Route, error) {
	kind, ref := req.GetTaskKind(), req.GetModel()
	if kind == "" && ref == "" {
		return workflows.Route{}, nil, nil
	}
	if kind != "" && !routing.ValidKind(kind) {
		return workflows.Route{}, nil, grpcstatus.Errorf(codes.InvalidArgument, "unknown task kind %q", kind)
	}
	if s.router == nil {
		return workflows.Route{}, nil, grpcstatus.Errorf(codes.FailedPrecondition, "model routing is unavailable: %v", s.routerErr)
	}
	var rec routing.Recommendation
	var err error
	if ref != "" {
		rec, err = s.router.Pin(ctx, ref, workerRunnable)
		if err != nil {
			return workflows.Route{}, nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		rec, err = s.router.Recommend(ctx, kind, workerRunnable)
		if errors.Is(err, routing.ErrNoRoute) {
			return workflows.Route{Kind: kind}, &orchdv1.Route{TaskKind: kind,
				Rationale: "no routable model for this kind; the agent's own model runs"}, nil
		}
		if err != nil {
			return workflows.Route{}, nil, err
		}
	}
	rec.Kind = kind
	return workflows.Route{Kind: kind, Provider: rec.Provider, Model: rec.Model, Effort: rec.Effort}, routePB(rec), nil
}

func routePB(rec routing.Recommendation) *orchdv1.Route {
	return &orchdv1.Route{
		TaskKind: rec.Kind, Provider: rec.Provider, Model: rec.Model, Effort: rec.Effort,
		Rationale: rec.Rationale, EvidenceN: clampInt32(rec.EvidenceN), Confidence: rec.Confidence,
	}
}

func (s *server) RecommendModel(ctx context.Context, req *orchdv1.RecommendModelRequest) (*orchdv1.RecommendModelResponse, error) {
	kind := req.GetTaskKind()
	if !routing.ValidKind(kind) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "unknown task kind %q", kind)
	}
	if s.router == nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "model routing is unavailable: %v", s.routerErr)
	}
	rec, err := s.router.Recommend(ctx, kind, nil)
	if errors.Is(err, routing.ErrNoRoute) {
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &orchdv1.RecommendModelResponse{Route: routePB(rec), FromTable: rec.FromTable}, nil
}

// newRouter builds the router over orch.db, or reports why routing is off.
func newRouter(store *sql.DB, s routing.Policy) (modelRouter, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &routing.Router{Store: routing.NewStore(store), Models: models.NewRegistry(store), Policy: s}, nil
}
