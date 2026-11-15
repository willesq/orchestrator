package cli

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// fakeRetag answers RetagSource for every fakeClient.
var fakeRetag = func(*orchdv1.RetagSourceRequest) (*orchdv1.RetagSourceResponse, error) {
	return &orchdv1.RetagSourceResponse{}, nil
}

func (f *fakeClient) RetagSource(_ context.Context, req *orchdv1.RetagSourceRequest, _ ...grpc.CallOption) (*orchdv1.RetagSourceResponse, error) {
	return fakeRetag(req)
}
