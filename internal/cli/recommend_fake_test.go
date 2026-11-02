package cli

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

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

// fakeReindex answers ReindexKeywords for every fakeClient.
var fakeReindex = func(*orchdv1.ReindexKeywordsRequest) (*orchdv1.ReindexKeywordsResponse, error) {
	return &orchdv1.ReindexKeywordsResponse{}, nil
}

func (f *fakeClient) ReindexKeywords(_ context.Context, req *orchdv1.ReindexKeywordsRequest, _ ...grpc.CallOption) (*orchdv1.ReindexKeywordsResponse, error) {
	return fakeReindex(req)
}

func (f *fakeClient) ListPRs(context.Context, *orchdv1.ListPRsRequest, ...grpc.CallOption) (*orchdv1.ListPRsResponse, error) {
	return &orchdv1.ListPRsResponse{}, nil
}

func (f *fakeClient) GetGitHubIdentity(context.Context, *orchdv1.GetGitHubIdentityRequest, ...grpc.CallOption) (*orchdv1.GitHubIdentity, error) {
	return fakeGitHub.identity, fakeGitHub.err
}

func (f *fakeClient) ListGitHubScopes(context.Context, *orchdv1.ListGitHubScopesRequest, ...grpc.CallOption) (*orchdv1.ListGitHubScopesResponse, error) {
	return fakeGitHub.scopes, fakeGitHub.err
}

func (f *fakeClient) SetGitHubScope(_ context.Context, req *orchdv1.SetGitHubScopeRequest, _ ...grpc.CallOption) (*orchdv1.SetGitHubScopeResponse, error) {
	fakeGitHub.set = append(fakeGitHub.set, req.GetScope())
	return &orchdv1.SetGitHubScopeResponse{Scope: req.GetScope()}, fakeGitHub.err
}

func (f *fakeClient) ListGitHubRepos(context.Context, *orchdv1.ListGitHubReposRequest, ...grpc.CallOption) (*orchdv1.ListGitHubReposResponse, error) {
	return fakeGitHub.repos, fakeGitHub.err
}

// fakeGitHub answers the GitHub RPCs for every fakeClient; tests that use it
// replace it and restore it.
var fakeGitHub = struct {
	identity *orchdv1.GitHubIdentity
	scopes   *orchdv1.ListGitHubScopesResponse
	repos    *orchdv1.ListGitHubReposResponse
	set      []*orchdv1.GitHubScope
	err      error
}{
	identity: &orchdv1.GitHubIdentity{},
	scopes:   &orchdv1.ListGitHubScopesResponse{},
	repos:    &orchdv1.ListGitHubReposResponse{},
}

func (f *fakeClient) ListGitHubItems(context.Context, *orchdv1.ListGitHubItemsRequest, ...grpc.CallOption) (*orchdv1.ListGitHubItemsResponse, error) {
	return &orchdv1.ListGitHubItemsResponse{}, nil
}

func (f *fakeClient) ListPRReviews(context.Context, *orchdv1.ListPRReviewsRequest, ...grpc.CallOption) (*orchdv1.ListPRReviewsResponse, error) {
	return &orchdv1.ListPRReviewsResponse{Reviews: prWatches.reviews}, prWatches.listErr
}

func (f *fakeClient) WatchPRReview(context.Context, *orchdv1.WatchPRReviewRequest, ...grpc.CallOption) (*orchdv1.PRReview, error) {
	return &orchdv1.PRReview{}, nil
}

func (f *fakeClient) StopPRReview(_ context.Context, req *orchdv1.StopPRReviewRequest, _ ...grpc.CallOption) (*orchdv1.StopPRReviewResponse, error) {
	if prWatches.stopErr != nil {
		return nil, prWatches.stopErr
	}
	prWatches.stopped = append(prWatches.stopped, req)
	return &orchdv1.StopPRReviewResponse{}, nil
}

func (f *fakeClient) GetPRDetail(context.Context, *orchdv1.GetPRDetailRequest, ...grpc.CallOption) (*orchdv1.PRDetail, error) {
	return &orchdv1.PRDetail{}, nil
}

func (f *fakeClient) TailLogs(_ context.Context, req *orchdv1.TailLogsRequest, _ ...grpc.CallOption) (*orchdv1.TailLogsResponse, error) {
	f.tailReqs = append(f.tailReqs, proto.Clone(req).(*orchdv1.TailLogsRequest))
	if f.err != nil {
		return nil, f.err
	}
	if len(f.tails) == 0 {
		return nil, f.tailErr
	}
	resp := f.tails[0]
	f.tails = f.tails[1:]
	return resp, nil
}

func (f *fakeClient) GetSystemMetrics(context.Context, *orchdv1.GetSystemMetricsRequest, ...grpc.CallOption) (*orchdv1.SystemMetrics, error) {
	return &orchdv1.SystemMetrics{}, f.err
}

func (f *fakeClient) ListDocs(context.Context, *orchdv1.ListDocsRequest, ...grpc.CallOption) (*orchdv1.ListDocsResponse, error) {
	return &orchdv1.ListDocsResponse{}, f.err
}

func (f *fakeClient) GetDocument(_ context.Context, req *orchdv1.GetDocumentRequest, _ ...grpc.CallOption) (*orchdv1.Document, error) {
	f.docReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.document, nil
}

func (f *fakeClient) ListRegisteredSources(context.Context, *orchdv1.ListRegisteredSourcesRequest, ...grpc.CallOption) (*orchdv1.ListRegisteredSourcesResponse, error) {
	return &orchdv1.ListRegisteredSourcesResponse{}, f.err
}

func (f *fakeClient) SaveSource(context.Context, *orchdv1.SaveSourceRequest, ...grpc.CallOption) (*orchdv1.RegisteredSource, error) {
	return &orchdv1.RegisteredSource{}, f.err
}

func (f *fakeClient) RemoveSource(context.Context, *orchdv1.RemoveSourceRequest, ...grpc.CallOption) (*orchdv1.RemoveSourceResponse, error) {
	return &orchdv1.RemoveSourceResponse{}, f.err
}

func (f *fakeClient) RefreshSource(_ context.Context, req *orchdv1.RefreshSourceRequest, _ ...grpc.CallOption) (*orchdv1.RefreshSourceResponse, error) {
	f.refreshReq = req
	return &orchdv1.RefreshSourceResponse{}, f.err
}

func (f *fakeClient) SyncDocs(_ context.Context, req *orchdv1.SyncDocsRequest, _ ...grpc.CallOption) (*orchdv1.DocsSyncState, error) {
	f.docsSyncReq = req
	if f.docsSyncErr != nil {
		return nil, f.docsSyncErr
	}
	return &orchdv1.DocsSyncState{}, f.err
}

func (f *fakeClient) GetDocsSync(context.Context, *orchdv1.GetDocsSyncRequest, ...grpc.CallOption) (*orchdv1.DocsSyncState, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.docsStates) == 0 {
		return &orchdv1.DocsSyncState{}, nil
	}
	st := f.docsStates[0]
	if len(f.docsStates) > 1 {
		f.docsStates = f.docsStates[1:]
	}
	return st, nil
}

func (f *fakeClient) GetSafetyNet(context.Context, *orchdv1.GetSafetyNetRequest, ...grpc.CallOption) (*orchdv1.SafetyNetState, error) {
	return &orchdv1.SafetyNetState{}, nil
}

func (f *fakeClient) InstallSafetyNet(context.Context, *orchdv1.InstallSafetyNetRequest, ...grpc.CallOption) (*orchdv1.SafetyNetState, error) {
	return &orchdv1.SafetyNetState{}, nil
}

// fakeBackfill answers BackfillSessions for every fakeClient.
var fakeBackfill = func(*orchdv1.BackfillSessionsRequest) (*orchdv1.BackfillSessionsResponse, error) {
	return &orchdv1.BackfillSessionsResponse{}, nil
}

func (f *fakeClient) BackfillSessions(_ context.Context, req *orchdv1.BackfillSessionsRequest, _ ...grpc.CallOption) (*orchdv1.BackfillSessionsResponse, error) {
	return fakeBackfill(req)
}
