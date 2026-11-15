package hook

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// The quarantine RPCs (SP-028), answered empty: these tests never call them.

func (f *fakeClient) ListQuarantine(context.Context, *orchdv1.ListQuarantineRequest, ...grpc.CallOption) (*orchdv1.ListQuarantineResponse, error) {
	return &orchdv1.ListQuarantineResponse{}, nil
}

func (f *fakeClient) PurgeQuarantine(context.Context, *orchdv1.PurgeQuarantineRequest, ...grpc.CallOption) (*orchdv1.PurgeQuarantineResponse, error) {
	return &orchdv1.PurgeQuarantineResponse{}, nil
}
