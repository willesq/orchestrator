import OrchKit

/// Stands in when the gRPC transport cannot even be built, so the tray still
/// shows the daemon as down instead of the app failing to launch. A feature
/// added since SP-081 conforms it to its API in the feature's own file
/// (AppClient.swift), so parallel features do not append to this one.
struct UnreachableDaemon: DaemonAPI, MemoryAPI, ControlAPI, TasksAPI, GitHubAPI, PRReviewAPI, SystemAPI, CatalogAPI,
    SafetyNetAPI, SettingsAPI {
    let reason: String

    /// What every call throws; feature files' conformances throw it too.
    var failure: DaemonControl.ControlError { .failed(status: -1, output: reason) }

    func status() async throws -> DaemonStatus { throw failure }
    func listPRs() async throws -> PRList { throw failure }
    func search(_ query: SearchQuery) async throws -> SearchResult { throw failure }
    func listSources(type: String, limit: Int) async throws -> SourceList { throw failure }
    func restart(_ name: String?) async throws -> [String] { throw failure }
    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] { throw failure }
    func task(id: String) async throws -> TaskRecord? { throw failure }
    func rate(id: String, _ rating: TaskRating) async throws { throw failure }
    func retry(id: String) async throws -> String { throw failure }
    func cancel(id: String) async throws -> String { throw failure }
    func watchTasks(status: TaskStatusFilter, limit: Int, onEvent: @escaping @Sendable (TaskEvent) async -> Void) async throws {
        throw failure
    }
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome { throw failure }
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) { throw failure }
    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity { throw failure }
    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList { throw failure }
    func requestGitHubSweep() async throws { throw failure }
    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope { throw failure }
    func gitHubRepos(owner: String) async throws -> [GitHubRepo] { throw failure }
    func gitHubItems() async throws -> GitHubItemList { throw failure }
    func prReviews() async throws -> [PRReview] { throw failure }
    func watchPRReview(url: String, mode: PRReviewMode, comment: String?, instructions: String?,
                       reviewAnyway: Bool, updateInPlace: Bool?) async throws -> PRReview {
        throw failure
    }
    func setPRReviewUpdateInPlace(url: String, on: Bool) async throws -> PRReview { throw failure }
    func stopPRReview(url: String, forget: Bool) async throws { throw failure }
    func prDetail(url: String, refresh: Bool) async throws -> PRDetail { throw failure }
    func tailLogs(after: UInt64, limit: Int) async throws -> LogPage { throw failure }
    func systemMetrics() async throws -> SystemMetrics { throw failure }
    func listDocs() async throws -> DocCatalog { throw failure }
    func syncDocs(owner: String?, force: Bool) async throws -> DocsSyncState { throw failure }
    func docsSync() async throws -> DocsSyncState { throw failure }
    func safetyNet() async throws -> SafetyNetState { throw failure }
    func installSafetyNet(rulesOnly: Bool) async throws -> SafetyNetState { throw failure }
    func settings() async throws -> SettingsListing { throw failure }
    func setSetting(_ ref: SettingRef, enabled: Bool) async throws -> SettingItem { throw failure }
    func addSetting(provider: String, kind: String, fields: [String: String]) async throws -> SettingItem { throw failure }
    func removeSetting(_ ref: SettingRef) async throws { throw failure }
    func document(uri: String) async throws -> RebuiltDocument { throw failure }
    func registeredSources() async throws -> [RegisteredSource] { throw failure }
    func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource { throw failure }
    func removeSource(id: String, purge: Bool) async throws -> PurgeResult { throw failure }
    func refreshSource(id: String?) async throws { throw failure }
}

extension UnreachableDaemon: ConfigAPI, DocsOwnersAPI {
    func config() async throws -> ConfigState { throw failure }
    func setConfig(_ values: [String: String], confirmSensitive: Bool) async throws -> ConfigState { throw failure }
    func removeLegacyConfig() async throws -> ConfigState { throw failure }
    func docsOwners() async throws -> [DocsOwner] { throw failure }
    func docsOwnerRepos(owner: String) async throws -> [DocsOwnerRepoChoice] { throw failure }
    func saveDocsOwner(owner: String, enabled: Bool, mode: DocsRepoMode, repos: [String]) async throws -> DocsOwner { throw failure }
    func removeDocsOwner(owner: String, purge: Bool) async throws -> DocsPurgeResult { throw failure }
    func purgeDocs(owner: String, repos: [String]) async throws -> DocsPurgeResult { throw failure }
}

extension UnreachableDaemon: SpacesAPI {
    func enforcement() async throws -> EnforcementState { throw failure }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult { throw failure }
    func explainPlacement(target: String) async throws -> PlacementExplanation { throw failure }
}

extension UnreachableDaemon: UnlocksAPI {
    func unlocks() async throws -> [OperatorUnlock] { throw failure }
    func confirmUnlock(id: String, allow: Bool) async throws { throw failure }
    func lockUnlock(id: String) async throws -> Int { throw failure }
}

extension UnreachableDaemon: SpacesOverviewAPI {
    func listSpaces() async throws -> SpacesListing { throw failure }
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] { throw failure }
    func toolLogSummary() async throws -> [SpaceActivity] { throw failure }
    func unconfirmedGitHubOrgs() async throws -> [String] { throw failure }
}

extension UnreachableDaemon: SpacesReviewAPI, SpacesActivityAPI {
    func diffSpaces() async throws -> SpacesPolicyDiff { throw failure }
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft { throw failure }
    func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64 { throw failure }
    func toolEvents(owner: String, afterSeq: Int64, limit: Int32) async throws -> ToolLogPage { throw failure }
}

extension UnreachableDaemon: SpacesRelabelAPI {
    func relabel(apply: Bool) async throws -> RelabelRun { throw failure }
}

extension UnreachableDaemon: PRLandingAPI {
    func landing(taskID: String) async throws -> PRLanding { throw failure }
    func landingDecisions() async throws -> [String: PRLanding.State] { throw failure }
    func land(taskID: String, sha: String) async throws -> PRLanding { throw failure }
    func undoLanding(taskID: String) async throws -> PRLanding { throw failure }
    func discardLanding(taskID: String, restore: Bool) async throws -> PRLanding { throw failure }
    func taskActivity(taskID: String) async throws -> TaskActivityLog { throw failure }
}
