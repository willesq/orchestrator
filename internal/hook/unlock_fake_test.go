package hook

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// The operator unlock RPCs (SP-077); nothing here calls them.

func (f *fakeClient) RequestUnlock(context.Context, *orchdv1.RequestUnlockRequest, ...grpc.CallOption) (*orchdv1.RequestUnlockResponse, error) {
	return &orchdv1.RequestUnlockResponse{}, nil
}

func (f *fakeClient) ListUnlocks(context.Context, *orchdv1.ListUnlocksRequest, ...grpc.CallOption) (*orchdv1.ListUnlocksResponse, error) {
	return &orchdv1.ListUnlocksResponse{}, nil
}

func (f *fakeClient) ConfirmUnlock(context.Context, *orchdv1.ConfirmUnlockRequest, ...grpc.CallOption) (*orchdv1.ConfirmUnlockResponse, error) {
	return &orchdv1.ConfirmUnlockResponse{}, nil
}

func (f *fakeClient) Lock(context.Context, *orchdv1.LockRequest, ...grpc.CallOption) (*orchdv1.LockResponse, error) {
	return &orchdv1.LockResponse{}, nil
}

func (f *fakeClient) ProveUnlock(context.Context, *orchdv1.ProveUnlockRequest, ...grpc.CallOption) (*orchdv1.ProveUnlockResponse, error) {
	return &orchdv1.ProveUnlockResponse{}, nil
}
