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

// fakeRate answers RateTask for every fakeClient.
var fakeRate = func(*orchdv1.RateTaskRequest) (*orchdv1.RateTaskResponse, error) {
	return &orchdv1.RateTaskResponse{}, nil
}

func (f *fakeClient) RateTask(_ context.Context, req *orchdv1.RateTaskRequest, _ ...grpc.CallOption) (*orchdv1.RateTaskResponse, error) {
	return fakeRate(req)
}

// fakeMigrate answers MigrateHybrid for every fakeClient.
var fakeMigrate = func(*orchdv1.MigrateHybridRequest) (*orchdv1.MigrateHybridResponse, error) {
	return &orchdv1.MigrateHybridResponse{}, nil
}

func (f *fakeClient) MigrateHybrid(_ context.Context, req *orchdv1.MigrateHybridRequest, _ ...grpc.CallOption) (*orchdv1.MigrateHybridResponse, error) {
	return fakeMigrate(req)
}

// fakeDropBackup answers DropBackup for every fakeClient.
var fakeDropBackup = func(*orchdv1.DropBackupRequest) (*orchdv1.DropBackupResponse, error) {
	return &orchdv1.DropBackupResponse{}, nil
}

func (f *fakeClient) DropBackup(_ context.Context, req *orchdv1.DropBackupRequest, _ ...grpc.CallOption) (*orchdv1.DropBackupResponse, error) {
	return fakeDropBackup(req)
}

func (f *fakeClient) ListPRs(context.Context, *orchdv1.ListPRsRequest, ...grpc.CallOption) (*orchdv1.ListPRsResponse, error) {
	return &orchdv1.ListPRsResponse{}, nil
}
