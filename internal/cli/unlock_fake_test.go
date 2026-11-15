package cli

import (
	"context"
	"time"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// unlockFake is what fakeClient's unlock RPCs (SP-077) answer and record.
var unlockFake struct {
	request    *orchdv1.RequestUnlockResponse
	requestErr error
	requestReq *orchdv1.RequestUnlockRequest
	lock       *orchdv1.LockResponse
	lockErr    error
	lockReqs   []*orchdv1.LockRequest
	// proveErrs answer ProveUnlock in turn; past the end it grants.
	proveErrs []error
	proveReqs []*orchdv1.ProveUnlockRequest
}

func (f *fakeClient) ProveUnlock(_ context.Context, req *orchdv1.ProveUnlockRequest, _ ...grpc.CallOption) (*orchdv1.ProveUnlockResponse, error) {
	unlockFake.proveReqs = append(unlockFake.proveReqs, req)
	if n := len(unlockFake.proveReqs); n <= len(unlockFake.proveErrs) && unlockFake.proveErrs[n-1] != nil {
		return nil, unlockFake.proveErrs[n-1]
	}
	return &orchdv1.ProveUnlockResponse{Unlock: &orchdv1.OperatorUnlock{
		Id: req.GetId(), State: "active", ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}}, nil
}

func (f *fakeClient) RequestUnlock(_ context.Context, req *orchdv1.RequestUnlockRequest, _ ...grpc.CallOption) (*orchdv1.RequestUnlockResponse, error) {
	unlockFake.requestReq = req
	return unlockFake.request, unlockFake.requestErr
}

func (f *fakeClient) ListUnlocks(context.Context, *orchdv1.ListUnlocksRequest, ...grpc.CallOption) (*orchdv1.ListUnlocksResponse, error) {
	return &orchdv1.ListUnlocksResponse{}, nil
}

func (f *fakeClient) ConfirmUnlock(context.Context, *orchdv1.ConfirmUnlockRequest, ...grpc.CallOption) (*orchdv1.ConfirmUnlockResponse, error) {
	return &orchdv1.ConfirmUnlockResponse{}, nil
}

func (f *fakeClient) Lock(_ context.Context, req *orchdv1.LockRequest, _ ...grpc.CallOption) (*orchdv1.LockResponse, error) {
	unlockFake.lockReqs = append(unlockFake.lockReqs, req)
	return unlockFake.lock, unlockFake.lockErr
}
