import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
@testable import OrchKit

/// Serves canned responses on a Unix socket, so
/// DaemonClient is exercised over a real gRPC connection without a running
/// orchd.
struct FakeOrchdService: RegistrableRPCService {
    var status = Orchd_V1_StatusResponse()
    var prs = Orchd_V1_ListPRsResponse()
    var search = Orchd_V1_SearchResponse()
    var sources = Orchd_V1_ListSourcesResponse()
    var tasks = Orchd_V1_ListTasksResponse()
    var dispatch = Orchd_V1_DispatchResponse()
    var recommend = Orchd_V1_RecommendModelResponse()
    var ghIdentity = Orchd_V1_GitHubIdentity()
    var ghScopes = Orchd_V1_ListGitHubScopesResponse()
    var ghRepos = Orchd_V1_ListGitHubReposResponse()
    var ghItems = Orchd_V1_ListGitHubItemsResponse()
    var prReviews = Orchd_V1_ListPRReviewsResponse()
    var prReview = Orchd_V1_PRReview()
    var prDetail = Orchd_V1_PRDetail()
    var logs = Orchd_V1_TailLogsResponse()
    var metrics = Orchd_V1_SystemMetrics()
    var docs = Orchd_V1_ListDocsResponse()
    var document = Orchd_V1_Document()
    var registered = Orchd_V1_ListRegisteredSourcesResponse()
    var safetyNet = Orchd_V1_SafetyNetState()
    var landing = Orchd_V1_PRLanding()
    var landingDecisions = Orchd_V1_ListPRLandingsResponse()
    var activity = Orchd_V1_TaskActivity()
    var settings = Orchd_V1_ListSettingsResponse()
    var settingItem = Orchd_V1_SettingItem()
    var configState = Orchd_V1_ConfigState()
    var docsOwners = Orchd_V1_ListDocsOwnersResponse()
    var docsOwnerRepos = Orchd_V1_ListDocsOwnerReposResponse()
    var docOwner = Orchd_V1_DocOwner()
    var purge = Orchd_V1_PurgeDocsResponse()
    var enforcement = Orchd_V1_EnforcementState()
    var setEnforcement = Orchd_V1_SetEnforcementResponse()
    /// ExplainPlacement's answer per target; any other target is refused.
    var placements: [String: Orchd_V1_PlacementExplanation] = [:]
    /// Records requests the fake received, for assertions.
    var recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let status = self.status, prs = self.prs, search = self.search, sources = self.sources
        let recorder = self.recorder
        let tasks = self.tasks, dispatch = self.dispatch, recommend = self.recommend
        let ghIdentity = self.ghIdentity, ghScopes = self.ghScopes, ghRepos = self.ghRepos, ghItems = self.ghItems
        let prReviews = self.prReviews, prReview = self.prReview, prDetail = self.prDetail
        let logs = self.logs, metrics = self.metrics
        let docs = self.docs, document = self.document, registered = self.registered
        let safetyNet = self.safetyNet
        let settings = self.settings, settingItem = self.settingItem
        let landing = self.landing, landingDecisions = self.landingDecisions, activity = self.activity
        let configState = self.configState, docsOwners = self.docsOwners, docsOwnerRepos = self.docsOwnerRepos
        let docOwner = self.docOwner, purge = self.purge
        let enforcement = self.enforcement, setEnforcement = self.setEnforcement, placements = self.placements
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetEnforcement.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetEnforcementRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_EnforcementState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: enforcement))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetEnforcement.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetEnforcementRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SetEnforcementResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: setEnforcement))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ExplainPlacement.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ExplainPlacementRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PlacementExplanation>()
        ) { request, _ in
            var target = ""
            for try await message in request.messages {
                recorder.record(message)
                target = message.target
            }
            guard let answer = placements[target] else {
                return StreamingServerResponse(single: ServerResponse(error: RPCError(code: .invalidArgument, message: "no fixture for \(target)")))
            }
            return StreamingServerResponse(single: ServerResponse(message: answer))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetConfig.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetConfigRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ConfigState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: configState))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetConfig.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetConfigRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ConfigState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: configState))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RemoveLegacyConfig.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RemoveLegacyConfigRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ConfigState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            var removed = configState
            removed.legacyFile = ""
            return StreamingServerResponse(single: ServerResponse(message: removed))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListDocsOwners.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListDocsOwnersRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListDocsOwnersResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: docsOwners))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListDocsOwnerRepos.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListDocsOwnerReposRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListDocsOwnerReposResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: docsOwnerRepos))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SaveDocsOwner.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SaveDocsOwnerRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_DocOwner>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: docOwner))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RemoveDocsOwner.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RemoveDocsOwnerRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PurgeDocsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: purge))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.PurgeDocs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_PurgeDocsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PurgeDocsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: purge))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetPRLanding.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_PRLandingRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRLanding>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: landing))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListPRLandings.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListPRLandingsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListPRLandingsResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: landingDecisions))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.LandPR.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_LandPRRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRLanding>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: landing))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.UndoPRLanding.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_PRLandingRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRLanding>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: landing))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.DiscardPRLanding.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_DiscardPRLandingRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRLanding>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: landing))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetTaskActivity.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetTaskActivityRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_TaskActivity>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: activity))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListSettings.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListSettingsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListSettingsResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: settings))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetSettingEnabled.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetSettingEnabledRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SettingItem>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: settingItem))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.AddSetting.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_AddSettingRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SettingItem>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: settingItem))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RemoveSetting.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RemoveSettingRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RemoveSettingResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_RemoveSettingResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetSafetyNet.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetSafetyNetRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SafetyNetState>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: safetyNet))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.InstallSafetyNet.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_InstallSafetyNetRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SafetyNetState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: safetyNet))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SyncDocs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SyncDocsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_DocsSyncState>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            var state = Orchd_V1_DocsSyncState()
            state.owner = "willesq"
            return StreamingServerResponse(single: ServerResponse(message: state))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetDocsSync.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetDocsSyncRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_DocsSyncState>()
        ) { _, _ in
            var state = Orchd_V1_DocsSyncState()
            state.running = true
            state.owner = "willesq"
            state.startedAt = 1_700_000_000
            state.done = 3
            state.total = 52
            state.repo = "airflow"
            return StreamingServerResponse(single: ServerResponse(message: state))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListDocs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListDocsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListDocsResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: docs))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetDocument.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetDocumentRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_Document>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: document))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListRegisteredSources.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListRegisteredSourcesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListRegisteredSourcesResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: registered))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SaveSource.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SaveSourceRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RegisteredSource>()
        ) { request, _ in
            var saved = Orchd_V1_RegisteredSource()
            for try await message in request.messages {
                recorder.record(message)
                saved = message.source
            }
            saved.id = "\(saved.kind)|\(saved.uri)"
            saved.status = "new"
            return StreamingServerResponse(single: ServerResponse(message: saved))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RemoveSource.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RemoveSourceRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RemoveSourceResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            var response = Orchd_V1_RemoveSourceResponse()
            response.purgedSources = 2
            response.purgedChunks = 9
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RefreshSource.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RefreshSourceRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RefreshSourceResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_RefreshSourceResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.TailLogs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_TailLogsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_TailLogsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: logs))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetSystemMetrics.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetSystemMetricsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SystemMetrics>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: metrics))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetGitHubIdentity.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetGitHubIdentityRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_GitHubIdentity>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: ghIdentity))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListGitHubScopes.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListGitHubScopesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListGitHubScopesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: ghScopes))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetGitHubScope.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetGitHubScopeRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SetGitHubScopeResponse>()
        ) { request, _ in
            var scope = Orchd_V1_GitHubScope()
            for try await message in request.messages {
                recorder.record(message)
                scope = message.scope
            }
            scope.kind = "org"
            var response = Orchd_V1_SetGitHubScopeResponse()
            response.scope = scope
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListGitHubRepos.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListGitHubReposRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListGitHubReposResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: ghRepos))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListGitHubItems.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListGitHubItemsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListGitHubItemsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: ghItems))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RequestGitHubSweep.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RequestGitHubSweepRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RequestGitHubSweepResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_RequestGitHubSweepResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListPRReviews.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListPRReviewsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListPRReviewsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: prReviews))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.WatchPRReview.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_WatchPRReviewRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRReview>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: prReview))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetPRReviewUpdateInPlace.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetPRReviewUpdateInPlaceRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRReview>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: prReview))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.StopPRReview.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_StopPRReviewRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_StopPRReviewResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_StopPRReviewResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetPRDetail.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetPRDetailRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_PRDetail>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: prDetail))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListTasks.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListTasksRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListTasksResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: tasks))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetTask.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetTaskRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_GetTaskResponse>()
        ) { request, _ in
            var id = ""
            for try await message in request.messages { id = message.id }
            var response = Orchd_V1_GetTaskResponse()
            if let task = tasks.tasks.first(where: { $0.id == id }) {
                response.task = task
                response.found = true
            }
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RateTask.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RateTaskRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RateTaskResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_RateTaskResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Dispatch.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_DispatchRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_DispatchResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: dispatch))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RecommendModel.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RecommendModelRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RecommendModelResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: recommend))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Search.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SearchRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SearchResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: search))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListSources.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListSourcesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListSourcesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: sources))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Restart.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RestartRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RestartResponse>()
        ) { request, _ in
            var name = ""
            for try await message in request.messages { name = message.name }
            guard name != "nope" else { throw RPCError(code: .notFound, message: "no subsystem nope") }
            var response = Orchd_V1_RestartResponse()
            response.restarted = name.isEmpty ? ["qdrant", "handoff"] : [name]
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Status.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_StatusRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_StatusResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: status))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListPRs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListPRsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListPRsResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: prs))
        }
    }
}

/// Runs `body` with a fake orchd listening at a fresh socket path.
func withFakeOrchd(
    _ service: FakeOrchdService,
    _ body: (String) async throws -> Void
) async throws {
    let dir = try TempDir()
    let socket = dir.url.appendingPathComponent("orchd.sock").path
    let server = GRPCServer(
        transport: HTTP2ServerTransport.Posix(
            address: .unixDomainSocket(path: socket),
            transportSecurity: .plaintext
        ),
        services: [service]
    )
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask { try await server.serve() }
        defer { server.beginGracefulShutdown() }
        let deadline = ContinuousClock.now + .seconds(5)
        while !FileManager.default.fileExists(atPath: socket) {
            guard ContinuousClock.now < deadline else {
                throw RPCError(code: .unavailable, message: "fake orchd never listened on \(socket)")
            }
            try await Task.sleep(for: .milliseconds(10))
        }
        try await body(socket)
    }
}

/// Collects the requests a fake service handled.
final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var messages: [any Sendable] = []

    func record(_ message: any Sendable) { lock.withLock { messages.append(message) } }

    func last<T>(_: T.Type) -> T? { lock.withLock { messages.last { $0 is T } as? T } }
}
