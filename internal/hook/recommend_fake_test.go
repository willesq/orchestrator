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

func (f *fakeClient) ReindexKeywords(context.Context, *orchdv1.ReindexKeywordsRequest, ...grpc.CallOption) (*orchdv1.ReindexKeywordsResponse, error) {
	return &orchdv1.ReindexKeywordsResponse{}, nil
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

func (f *fakeClient) RequestGitHubSweep(context.Context, *orchdv1.RequestGitHubSweepRequest, ...grpc.CallOption) (*orchdv1.RequestGitHubSweepResponse, error) {
	return &orchdv1.RequestGitHubSweepResponse{}, nil
}

func (f *fakeClient) ListPRReviews(context.Context, *orchdv1.ListPRReviewsRequest, ...grpc.CallOption) (*orchdv1.ListPRReviewsResponse, error) {
	return &orchdv1.ListPRReviewsResponse{}, nil
}

func (f *fakeClient) WatchPRReview(context.Context, *orchdv1.WatchPRReviewRequest, ...grpc.CallOption) (*orchdv1.PRReview, error) {
	return &orchdv1.PRReview{}, nil
}

func (f *fakeClient) SetPRReviewUpdateInPlace(context.Context, *orchdv1.SetPRReviewUpdateInPlaceRequest, ...grpc.CallOption) (*orchdv1.PRReview, error) {
	return &orchdv1.PRReview{}, nil
}

func (f *fakeClient) StopPRReview(context.Context, *orchdv1.StopPRReviewRequest, ...grpc.CallOption) (*orchdv1.StopPRReviewResponse, error) {
	return &orchdv1.StopPRReviewResponse{}, nil
}

func (f *fakeClient) GetPRDetail(context.Context, *orchdv1.GetPRDetailRequest, ...grpc.CallOption) (*orchdv1.PRDetail, error) {
	return &orchdv1.PRDetail{}, nil
}

func (f *fakeClient) TailLogs(context.Context, *orchdv1.TailLogsRequest, ...grpc.CallOption) (*orchdv1.TailLogsResponse, error) {
	return &orchdv1.TailLogsResponse{}, nil
}

func (f *fakeClient) GetSystemMetrics(context.Context, *orchdv1.GetSystemMetricsRequest, ...grpc.CallOption) (*orchdv1.SystemMetrics, error) {
	return &orchdv1.SystemMetrics{}, nil
}

func (f *fakeClient) ListDocs(context.Context, *orchdv1.ListDocsRequest, ...grpc.CallOption) (*orchdv1.ListDocsResponse, error) {
	return &orchdv1.ListDocsResponse{}, nil
}

func (f *fakeClient) GetDocument(context.Context, *orchdv1.GetDocumentRequest, ...grpc.CallOption) (*orchdv1.Document, error) {
	return &orchdv1.Document{}, nil
}

func (f *fakeClient) ListRegisteredSources(context.Context, *orchdv1.ListRegisteredSourcesRequest, ...grpc.CallOption) (*orchdv1.ListRegisteredSourcesResponse, error) {
	return &orchdv1.ListRegisteredSourcesResponse{}, nil
}

func (f *fakeClient) SaveSource(context.Context, *orchdv1.SaveSourceRequest, ...grpc.CallOption) (*orchdv1.RegisteredSource, error) {
	return &orchdv1.RegisteredSource{}, nil
}

func (f *fakeClient) RemoveSource(context.Context, *orchdv1.RemoveSourceRequest, ...grpc.CallOption) (*orchdv1.RemoveSourceResponse, error) {
	return &orchdv1.RemoveSourceResponse{}, nil
}

func (f *fakeClient) RefreshSource(context.Context, *orchdv1.RefreshSourceRequest, ...grpc.CallOption) (*orchdv1.RefreshSourceResponse, error) {
	return &orchdv1.RefreshSourceResponse{}, nil
}

func (f *fakeClient) SyncDocs(context.Context, *orchdv1.SyncDocsRequest, ...grpc.CallOption) (*orchdv1.DocsSyncState, error) {
	return &orchdv1.DocsSyncState{}, nil
}

func (f *fakeClient) GetDocsSync(context.Context, *orchdv1.GetDocsSyncRequest, ...grpc.CallOption) (*orchdv1.DocsSyncState, error) {
	return &orchdv1.DocsSyncState{}, nil
}

func (f *fakeClient) GetSafetyNet(context.Context, *orchdv1.GetSafetyNetRequest, ...grpc.CallOption) (*orchdv1.SafetyNetState, error) {
	return &orchdv1.SafetyNetState{}, nil
}

func (f *fakeClient) InstallSafetyNet(context.Context, *orchdv1.InstallSafetyNetRequest, ...grpc.CallOption) (*orchdv1.SafetyNetState, error) {
	return &orchdv1.SafetyNetState{}, nil
}

func (f *fakeClient) ListSettings(context.Context, *orchdv1.ListSettingsRequest, ...grpc.CallOption) (*orchdv1.ListSettingsResponse, error) {
	return &orchdv1.ListSettingsResponse{}, nil
}

func (f *fakeClient) SetSettingEnabled(context.Context, *orchdv1.SetSettingEnabledRequest, ...grpc.CallOption) (*orchdv1.SettingItem, error) {
	return &orchdv1.SettingItem{}, nil
}

func (f *fakeClient) AddSetting(context.Context, *orchdv1.AddSettingRequest, ...grpc.CallOption) (*orchdv1.SettingItem, error) {
	return &orchdv1.SettingItem{}, nil
}

func (f *fakeClient) RemoveSetting(context.Context, *orchdv1.RemoveSettingRequest, ...grpc.CallOption) (*orchdv1.RemoveSettingResponse, error) {
	return &orchdv1.RemoveSettingResponse{}, nil
}

func (f *fakeClient) GetTaskActivity(context.Context, *orchdv1.GetTaskActivityRequest, ...grpc.CallOption) (*orchdv1.TaskActivity, error) {
	return &orchdv1.TaskActivity{}, nil
}

func (f *fakeClient) GetPRLanding(context.Context, *orchdv1.PRLandingRequest, ...grpc.CallOption) (*orchdv1.PRLanding, error) {
	return &orchdv1.PRLanding{}, nil
}

func (f *fakeClient) ListPRLandings(context.Context, *orchdv1.ListPRLandingsRequest, ...grpc.CallOption) (*orchdv1.ListPRLandingsResponse, error) {
	return &orchdv1.ListPRLandingsResponse{}, nil
}

func (f *fakeClient) LandPR(context.Context, *orchdv1.LandPRRequest, ...grpc.CallOption) (*orchdv1.PRLanding, error) {
	return &orchdv1.PRLanding{}, nil
}

func (f *fakeClient) UndoPRLanding(context.Context, *orchdv1.PRLandingRequest, ...grpc.CallOption) (*orchdv1.PRLanding, error) {
	return &orchdv1.PRLanding{}, nil
}

func (f *fakeClient) DiscardPRLanding(context.Context, *orchdv1.DiscardPRLandingRequest, ...grpc.CallOption) (*orchdv1.PRLanding, error) {
	return &orchdv1.PRLanding{}, nil
}

func (f *fakeClient) BackfillSessions(context.Context, *orchdv1.BackfillSessionsRequest, ...grpc.CallOption) (*orchdv1.BackfillSessionsResponse, error) {
	return &orchdv1.BackfillSessionsResponse{}, nil
}

func (f *fakeClient) ListDocsOwners(context.Context, *orchdv1.ListDocsOwnersRequest, ...grpc.CallOption) (*orchdv1.ListDocsOwnersResponse, error) {
	return &orchdv1.ListDocsOwnersResponse{}, nil
}

func (f *fakeClient) ListDocsOwnerRepos(context.Context, *orchdv1.ListDocsOwnerReposRequest, ...grpc.CallOption) (*orchdv1.ListDocsOwnerReposResponse, error) {
	return &orchdv1.ListDocsOwnerReposResponse{}, nil
}

func (f *fakeClient) SaveDocsOwner(context.Context, *orchdv1.SaveDocsOwnerRequest, ...grpc.CallOption) (*orchdv1.DocOwner, error) {
	return &orchdv1.DocOwner{}, nil
}

func (f *fakeClient) RemoveDocsOwner(context.Context, *orchdv1.RemoveDocsOwnerRequest, ...grpc.CallOption) (*orchdv1.PurgeDocsResponse, error) {
	return &orchdv1.PurgeDocsResponse{}, nil
}

func (f *fakeClient) PurgeDocs(context.Context, *orchdv1.PurgeDocsRequest, ...grpc.CallOption) (*orchdv1.PurgeDocsResponse, error) {
	return &orchdv1.PurgeDocsResponse{}, nil
}

func (f *fakeClient) GetConfig(context.Context, *orchdv1.GetConfigRequest, ...grpc.CallOption) (*orchdv1.ConfigState, error) {
	return &orchdv1.ConfigState{}, nil
}

func (f *fakeClient) SetConfig(context.Context, *orchdv1.SetConfigRequest, ...grpc.CallOption) (*orchdv1.ConfigState, error) {
	return &orchdv1.ConfigState{}, nil
}
