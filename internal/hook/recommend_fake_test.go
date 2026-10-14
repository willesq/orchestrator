package hook

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// fakeRecommend answers RecommendModel for every fakeClient; tests that
// exercise it set it and restore it.
var fakeRecommend = func(*orchdv1.RecommendModelRequest) (*orchdv1.RecommendModelResponse, error) {
	return &orchdv1.RecommendModelResponse{}, nil
}

func (f *fakeClient) RecommendModel(_ context.Context, req *orchdv1.RecommendModelRequest, _ ...grpc.CallOption) (*orchdv1.RecommendModelResponse, error) {
	return fakeRecommend(req)
}
