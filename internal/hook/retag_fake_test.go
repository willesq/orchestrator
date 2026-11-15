package hook

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (f *fakeClient) RetagSource(context.Context, *orchdv1.RetagSourceRequest, ...grpc.CallOption) (*orchdv1.RetagSourceResponse, error) {
	return &orchdv1.RetagSourceResponse{}, nil
}
