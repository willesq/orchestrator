package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

func (s *server) SetSpacesDraft(ctx context.Context, req *orchdv1.SetSpacesDraftRequest) (*orchdv1.SpacesDraft, error) {
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	p, err := spaces.Parse([]byte(req.GetPolicyJson()))
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	// Reason: agents may propose a policy (D-25), but the operator must be
	// able to tell their drafts from their own when they review and apply.
	_, byOperator := operator(ctx)
	r, problems, err := s.spaces.SaveDraft(ctx, p, callerLabel(ctx), byOperator)
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	log.Printf("orchd: spaces draft revision %d saved by %s", r.Number, callerLabel(ctx))
	return &orchdv1.SpacesDraft{Revision: r.Number, Sha256: r.SHA256, Problems: problemsProto(problems)}, nil
}

func (s *server) DiffSpaces(ctx context.Context, _ *orchdv1.DiffSpacesRequest) (*orchdv1.SpacesDiff, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_DiffSpaces_FullMethodName, "read the spaces policy"); err != nil {
		return nil, err
	}
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	d, err := s.spaces.Diff(ctx)
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	out := &orchdv1.SpacesDiff{Problems: problemsProto(d.Problems)}
	if d.Applied != nil {
		out.AppliedRevision, out.AppliedJson = d.Applied.Number, string(d.Applied.Doc)
	}
	if r := d.Draft; r != nil {
		out.DraftRevision, out.DraftJson = r.Number, string(r.Doc)
		out.DraftCreatedBy, out.DraftCreatedAt, out.DraftByOperator = r.CreatedBy, r.CreatedAt.Unix(), r.ByOperator
	}
	for _, c := range d.Changes {
		out.Changes = append(out.Changes, &orchdv1.SpacesChange{Space: c.Space, Kind: c.Kind, Fields: c.Fields, Details: c.Details})
	}
	return out, nil
}

func (s *server) ApplySpaces(ctx context.Context, req *orchdv1.ApplySpacesRequest) (*orchdv1.ApplySpacesResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ApplySpaces_FullMethodName, "apply the spaces policy"); err != nil {
		return nil, err
	}
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	if req.GetReviewedRevision() <= 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name the spaces revision you reviewed")
	}
	r, err := s.spaces.Apply(ctx, req.GetReviewedRevision(), callerLabel(ctx), req.GetAcceptAgentDraft())
	var problems spaces.Problems
	switch {
	case errors.Is(err, spaces.ErrStale):
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error()+"; review the newest draft and apply it")
	case errors.Is(err, spaces.ErrAgentDraft):
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error()+"; review its values, then apply it with accept_agent_draft (orch spaces apply --accept-agent-draft)")
	case errors.Is(err, spaces.ErrNoRevision):
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	case errors.As(err, &problems), errors.Is(err, spaces.ErrTampered):
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	case err != nil:
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	log.Printf("orchd: spaces revision %d applied by %s", r.Number, callerLabel(ctx))
	return &orchdv1.ApplySpacesResponse{AppliedRevision: r.Number}, nil
}

// spacesStatus is the spaces part of Status; nil when spaces are unavailable.
func (s *server) spacesStatus(ctx context.Context) *orchdv1.SpacesStatus {
	if s.spaces == nil {
		return nil
	}
	st, err := s.spaces.Status(ctx)
	if err != nil {
		return &orchdv1.SpacesStatus{Error: "could not read the spaces policy: " + err.Error()}
	}
	return &orchdv1.SpacesStatus{
		Configured:      st.Configured,
		AppliedRevision: st.AppliedRevision,
		DraftRevision:   st.DraftRevision,
		Drift:           st.Drift,
		Error:           st.Error,
	}
}

func problemsProto(ps spaces.Problems) []*orchdv1.SpacesProblem {
	out := make([]*orchdv1.SpacesProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, &orchdv1.SpacesProblem{Rule: string(p.Rule), Space: p.Space, Message: p.Message})
	}
	return out
}

// callerLabel records who saved or applied a revision, for created_by,
// applied_by and the log.
func callerLabel(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "unidentified process"
	}
	a, ok := p.AuthInfo.(callerAuth)
	switch {
	case !ok || a.err != nil:
		return "unidentified process"
	case a.spawned:
		return fmt.Sprintf("process %d started by orchd", a.pid)
	case a.harness != "":
		return fmt.Sprintf("process %d in %s", a.pid, a.harness)
	}
	return fmt.Sprintf("operator (process %d)", a.pid)
}
