import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2

/// The orchd calls the app depends on, behind a protocol so views and the
/// monitor can be tested without a daemon.
public protocol DaemonAPI: Sendable {
    func status() async throws -> DaemonStatus
    /// Every cached PR, most urgent first.
    func listPRs() async throws -> PRList
}

/// Talks to orchd over its Unix socket. One long-lived HTTP/2 connection is
/// shared by every call; the transport reconnects on its own when orchd
/// restarts and recreates the socket.
public final class DaemonClient: DaemonAPI, MemoryAPI, ControlAPI, TasksAPI, GitHubAPI, PRReviewAPI, SystemAPI, CatalogAPI,
    Sendable {
    private let client: GRPCClient<HTTP2ClientTransport.Posix>
    private let orchd: Orchd_V1_Orchd.Client<HTTP2ClientTransport.Posix>
    private let callTimeout: Duration
    private let searchTimeout: Duration

    /// `searchTimeout` is longer because the first search after a restart
    /// loads the embedding model.
    public init(
        socketPath: String = OrchPaths.socketPath(), callTimeout: Duration = .seconds(3),
        searchTimeout: Duration = .seconds(15)
    ) throws {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: socketPath),
            transportSecurity: .plaintext
        )
        client = GRPCClient(transport: transport)
        orchd = Orchd_V1_Orchd.Client(wrapping: client)
        self.callTimeout = callTimeout
        self.searchTimeout = searchTimeout
    }

    /// Runs the connection until `close()` or task cancellation. Call once,
    /// from a long-lived task, before issuing RPCs.
    public func run() async throws {
        try await client.runConnections()
    }

    public func close() {
        client.beginGracefulShutdown()
    }

    public func status() async throws -> DaemonStatus {
        let response = try await orchd.status(Orchd_V1_StatusRequest(), options: callOptions)
        return DaemonStatus(
            version: response.version,
            uptimeSeconds: response.uptimeSeconds,
            subsystems: response.subsystems.map {
                Subsystem(name: $0.name, state: $0.state, detail: $0.detail, error: $0.error, restarts: Int($0.restarts),
                          startedAt: date(fromUnix: $0.startedAt), lastBeat: date(fromUnix: $0.lastBeat))
            }
        )
    }

    public func listPRs() async throws -> PRList {
        let response = try await orchd.listPRs(Orchd_V1_ListPRsRequest(), options: callOptions)
        return PRList(
            prs: response.prs.map {
                PullRequest(
                    url: $0.url, title: $0.title, repo: $0.repo, ciState: $0.ciState,
                    reviewDecision: $0.reviewDecision, mergeable: $0.mergeable, isDraft: $0.isDraft,
                    reasons: $0.reasons, attention: Attention(rawValue: $0.attention)
                )
            },
            updatedAt: response.updatedAt > 0 ? Date(timeIntervalSince1970: TimeInterval(response.updatedAt)) : nil
        )
    }

    public func search(_ query: SearchQuery) async throws -> SearchResult {
        let q = query.normalized
        var request = Orchd_V1_SearchRequest()
        request.query = q.text
        request.topK = Int32(q.limit)
        request.sourceType = q.sourceType
        request.scope = q.scope
        request.mode = q.mode.rawValue
        request.sourceUri = q.sourceURI
        request.allTags = q.allTags
        var options = CallOptions.defaults
        options.timeout = searchTimeout
        let response = try await orchd.search(request, options: options)
        return SearchResult(
            hits: response.hits.map {
                SearchHit(
                    text: $0.text, sourceURI: $0.sourceUri, sourceType: $0.sourceType, scope: $0.scope,
                    score: $0.score, keywordScore: $0.keywordScore, exact: $0.exact, tags: $0.tags,
                    startedAt: date(fromUnix: $0.startedAt), ingestedAt: date(fromUnix: $0.ingestedAt)
                )
            },
            fallback: response.fallback.isEmpty ? nil : response.fallback
        )
    }

    public func listSources(type: String, limit: Int) async throws -> SourceList {
        var request = Orchd_V1_ListSourcesRequest()
        request.sourceType = type
        request.limit = Int32(clamping: max(limit, 0))
        let response = try await orchd.listSources(request, options: callOptions)
        return SourceList(
            sources: response.sources.map {
                SourceSummary(
                    uri: $0.sourceUri, sourceType: $0.sourceType, scope: $0.scope, chunks: Int($0.chunks),
                    tags: $0.tags, ingestedAt: date(fromUnix: $0.ingestedAt)
                )
            },
            totalSources: Int(response.totalSources),
            totalChunks: Int(response.totalChunks)
        )
    }

    public func restart(_ name: String?) async throws -> [String] {
        var request = Orchd_V1_RestartRequest()
        request.name = name ?? ""
        // Stopping a unit can take up to 15 s before the supervisor gives up.
        var options = CallOptions.defaults
        options.timeout = .seconds(40)
        return try await orchd.restart(request, options: options).restarted
    }

    public func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] {
        var request = Orchd_V1_ListTasksRequest()
        request.status = status.rawValue
        request.limit = Int32(clamping: max(limit, 0))
        return try await orchd.listTasks(request, options: callOptions).tasks.map(Self.task)
    }

    public func task(id: String) async throws -> TaskRecord? {
        var request = Orchd_V1_GetTaskRequest()
        request.id = id
        let response = try await orchd.getTask(request, options: callOptions)
        return response.found ? Self.task(response.task) : nil
    }

    public func rate(id: String, _ rating: TaskRating) async throws {
        var request = Orchd_V1_RateTaskRequest()
        request.id = id
        request.rating = rating.rawValue
        _ = try await orchd.rateTask(request, options: callOptions)
    }

    public func retry(id: String) async throws -> String {
        var request = Orchd_V1_RetryTaskRequest()
        request.id = id
        return try await orchd.retryTask(request, options: callOptions).runID
    }

    public func cancel(id: String) async throws -> String {
        var request = Orchd_V1_CancelTaskRequest()
        request.id = id
        return try await orchd.cancelTask(request, options: callOptions).outcome
    }

    /// Long-lived, so no timeout: it ends when orchd ends it (a restart, say)
    /// or the caller's task is cancelled.
    public func watchTasks(status: TaskStatusFilter, limit: Int,
                           onEvent: @escaping @Sendable (TaskEvent) async -> Void) async throws {
        var request = Orchd_V1_WatchTasksRequest()
        request.status = status.rawValue
        request.limit = Int32(clamping: max(limit, 0))
        try await orchd.watchTasks(request) { response in
            for try await message in response.messages {
                guard let kind = TaskEvent.Kind(rawValue: message.kind) else { continue }
                await onEvent(TaskEvent(kind: kind, tasks: message.tasks.map(Self.task)))
            }
        }
    }

    public func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome {
        var request = Orchd_V1_DispatchRequest()
        request.agent = draft.agent
        request.body = draft.body
        request.cwd = draft.cwd
        request.taskKind = draft.taskKind
        request.model = draft.model
        // Routing may build a live proposal first, which can take a while.
        var options = CallOptions.defaults
        options.timeout = .seconds(30)
        let response = try await orchd.dispatch(request, options: options)
        return DispatchOutcome(
            id: response.instanceID, workerEnabled: response.workerEnabled,
            route: response.hasRoute ? Self.route(response.route) : nil
        )
    }

    public func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) {
        var request = Orchd_V1_RecommendModelRequest()
        request.taskKind = taskKind
        let response = try await orchd.recommendModel(request, options: callOptions)
        return (response.hasRoute ? Self.route(response.route) : nil, response.fromTable)
    }

    static func task(_ t: Orchd_V1_Task) -> TaskRecord {
        TaskRecord(
            id: t.id, agent: t.agent, body: t.body, result: t.result, status: t.status, updatedAt: t.updatedAt,
            costUSD: t.costUsd, durationMS: t.durationMs, turns: Int(t.turns), sessionID: t.sessionID,
            models: t.models, prs: t.prs, verification: t.verification,
            route: t.hasRoute ? route(t.route) : nil, rating: TaskRating(rawValue: t.rating), prOutcome: t.prOutcome,
            run: t.hasRun ? run(t.run) : nil
        )
    }

    static func run(_ r: Orchd_V1_TaskRun) -> TaskRunInfo {
        TaskRunInfo(state: r.state, pid: Int(r.pid), startedAt: r.startedAt > 0 ? Date(timeIntervalSince1970: TimeInterval(r.startedAt)) : nil,
                    lastActivity: r.lastActivity > 0 ? Date(timeIntervalSince1970: TimeInterval(r.lastActivity)) : nil,
                    sessionID: r.sessionID, cwd: r.cwd, resumeCommand: r.resumeCommand)
    }

    static func route(_ r: Orchd_V1_Route) -> RouteInfo {
        RouteInfo(taskKind: r.taskKind, provider: r.provider, model: r.model, effort: r.effort,
                  rationale: r.rationale, evidenceCount: Int(r.evidenceN), confidence: r.confidence)
    }

    /// GitHub calls run gh in orchd, so they get longer than daemon-only calls.
    private var gitHubOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = .seconds(60)
        return options
    }

    public func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity {
        var request = Orchd_V1_GetGitHubIdentityRequest()
        request.refresh = refresh
        let r = try await orchd.getGitHubIdentity(request, options: gitHubOptions)
        return GitHubIdentity(login: r.login, orgs: r.orgs,
                              teams: r.teams.map { GitHubTeam(org: $0.org, slug: $0.slug, name: $0.name) })
    }

    public func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList {
        var request = Orchd_V1_ListGitHubScopesRequest()
        request.refresh = refresh
        let r = try await orchd.listGitHubScopes(request, options: gitHubOptions)
        return GitHubScopeList(scopes: r.scopes.map(Self.scope), warning: r.warning.isEmpty ? nil : r.warning)
    }

    public func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope {
        var pb = Orchd_V1_GitHubScope()
        pb.owner = scope.owner
        pb.enabled = scope.enabled
        pb.items = scope.items.rawValue
        pb.repoMode = scope.repoMode.rawValue
        pb.repos = scope.repoMode == .all ? [] : scope.repos
        var request = Orchd_V1_SetGitHubScopeRequest()
        request.scope = pb
        // Enabling a scope can make orchd check the user's memberships through gh.
        return Self.scope(try await orchd.setGitHubScope(request, options: gitHubOptions).scope)
    }

    public func gitHubRepos(owner: String) async throws -> [GitHubRepo] {
        var request = Orchd_V1_ListGitHubReposRequest()
        request.owner = owner
        return try await orchd.listGitHubRepos(request, options: gitHubOptions).repos.map {
            GitHubRepo(name: $0.name, isPrivate: $0.`private`, isArchived: $0.archived)
        }
    }

    public func gitHubItems() async throws -> GitHubItemList {
        let r = try await orchd.listGitHubItems(Orchd_V1_ListGitHubItemsRequest(), options: callOptions)
        let iso = ISO8601DateFormatter()
        return GitHubItemList(
            items: r.items.map {
                GitHubItem(
                    url: $0.url, title: $0.title, repo: $0.repo, isPR: $0.kind != "issue", number: Int($0.number),
                    author: $0.author, updatedAt: iso.date(from: $0.updatedAt), isDraft: $0.isDraft,
                    ciState: $0.ciState, reasons: $0.reasons.compactMap(GitHubReason.init(rawValue:)),
                    attention: Attention(rawValue: $0.attention)
                )
            },
            updatedAt: date(fromUnix: r.updatedAt)
        )
    }

    public func prReviews() async throws -> [PRReview] {
        try await orchd.listPRReviews(Orchd_V1_ListPRReviewsRequest(), options: callOptions).reviews.map(Self.review)
    }

    public func watchPRReview(url: String, mode: PRReviewMode, comment: String?, instructions: String?,
                              reviewAnyway: Bool) async throws -> PRReview {
        var request = Orchd_V1_WatchPRReviewRequest()
        request.url = url
        request.mode = mode.rawValue
        if let comment {
            request.setComment = true
            request.comment = comment
        }
        if let instructions {
            request.setInstructions = true
            request.instructions = instructions
        }
        request.reviewAnyway = reviewAnyway
        return Self.review(try await orchd.watchPRReview(request, options: callOptions))
    }

    public func stopPRReview(url: String, forget: Bool) async throws {
        var request = Orchd_V1_StopPRReviewRequest()
        request.url = url
        request.forget = forget
        _ = try await orchd.stopPRReview(request, options: callOptions)
    }

    public func landing(taskID: String) async throws -> PRLanding {
        var request = Orchd_V1_PRLandingRequest()
        request.taskID = taskID
        return Self.landing(try await orchd.getPRLanding(request, options: landingOptions))
    }

    public func landingDecisions() async throws -> [String: PRLanding.State] {
        let response = try await orchd.listPRLandings(Orchd_V1_ListPRLandingsRequest(), options: callOptions)
        return response.decisions.reduce(into: [:]) { out, decision in
            if let state = PRLanding.State(rawValue: decision.state) { out[decision.taskID] = state }
        }
    }

    public func land(taskID: String, sha: String) async throws -> PRLanding {
        var request = Orchd_V1_LandPRRequest()
        request.taskID = taskID
        request.sha = sha
        return Self.landing(try await orchd.landPR(request, options: landingOptions))
    }

    public func undoLanding(taskID: String) async throws -> PRLanding {
        var request = Orchd_V1_PRLandingRequest()
        request.taskID = taskID
        return Self.landing(try await orchd.undoPRLanding(request, options: landingOptions))
    }

    public func discardLanding(taskID: String, restore: Bool) async throws -> PRLanding {
        var request = Orchd_V1_DiscardPRLandingRequest()
        request.taskID = taskID
        request.restore = restore
        return Self.landing(try await orchd.discardPRLanding(request, options: landingOptions))
    }

    public func taskActivity(taskID: String) async throws -> TaskActivityLog {
        var request = Orchd_V1_GetTaskActivityRequest()
        request.id = taskID
        let response = try await orchd.getTaskActivity(request, options: callOptions)
        let iso = ISO8601DateFormatter()
        iso.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let steps = response.steps.enumerated().map { index, step in
            TaskActivityStep(index: index, at: iso.date(from: step.at), kind: step.kind, text: step.text, risk: step.risk)
        }
        return TaskActivityLog(available: response.available, steps: steps, total: Int(response.total))
    }

    /// A landing fetches a repository and may push over the network.
    private var landingOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = .seconds(180)
        return options
    }

    static func landing(_ l: Orchd_V1_PRLanding) -> PRLanding {
        var out = PRLanding(taskID: l.taskID, state: PRLanding.State(rawValue: l.state) ?? .none)
        out.url = l.url
        out.problem = l.problem
        out.path = l.path
        out.branch = l.branch
        out.baseSHA = l.baseSha
        out.sha = l.sha
        out.commits = l.commits.map { .init(sha: $0.sha, subject: $0.subject, author: $0.author) }
        out.totalCommits = Int(l.totalCommits)
        out.files = l.files.map { .init(path: $0.path, additions: Int($0.additions), deletions: Int($0.deletions)) }
        out.totalFiles = Int(l.totalFiles)
        out.additions = Int(l.additions)
        out.deletions = Int(l.deletions)
        out.binaryFiles = Int(l.binaryFiles)
        out.diff = l.diff
        out.diffTruncated = l.diffTruncated
        out.canLand = l.canLand
        out.blocked = l.blocked
        out.canUndo = l.canUndo
        out.revertSHA = l.revertSha
        out.decidedAt = l.decidedAt > 0 ? Date(timeIntervalSince1970: TimeInterval(l.decidedAt)) : nil
        return out
    }

    public func prDetail(url: String, refresh: Bool) async throws -> PRDetail {
        var request = Orchd_V1_GetPRDetailRequest()
        request.url = url
        request.refresh = refresh
        return Self.detail(try await orchd.getPRDetail(request, options: gitHubOptions))
    }

    static func review(_ r: Orchd_V1_PRReview) -> PRReview {
        PRReview(
            url: r.url, repo: r.repo, number: Int(r.number), mode: PRReviewMode(rawValue: r.mode) ?? .verdict,
            state: PRReviewState(rawValue: r.state), headSHA: r.headSha, pendingSHA: r.pendingSha, taskID: r.taskID,
            verdict: PRVerdict(rawValue: r.verdict), reason: r.reason, comment: r.comment, instructions: r.instructions, goAhead: r.goAhead,
            forUser: r.forUser, report: r.report,
            posted: r.posted,
            note: r.note, verdictAt: date(fromUnix: r.verdictAt), updatedAt: date(fromUnix: r.updatedAt),
            checkedAt: date(fromUnix: r.checkedAt), nextCheckAt: date(fromUnix: r.nextCheckAt)
        )
    }

    static func detail(_ d: Orchd_V1_PRDetail) -> PRDetail {
        let iso = ISO8601DateFormatter()
        return PRDetail(
            url: d.url, title: d.title, body: d.body, author: d.author, state: d.state, isDraft: d.isDraft,
            baseRef: d.baseRef, headRef: d.headRef, headSHA: d.headSha, additions: Int(d.additions),
            deletions: Int(d.deletions), changedFiles: Int(d.changedFiles), createdAt: iso.date(from: d.createdAt),
            updatedAt: iso.date(from: d.updatedAt), mergeable: d.mergeable, reviewDecision: d.reviewDecision,
            ciState: d.ciState, labels: d.labels, requestedReviewers: d.requestedReviewers,
            reviews: d.reviews.map { PRDetail.Reviewer(login: $0.login, state: $0.state) },
            checks: d.checks.map { PRDetail.Check(name: $0.name, result: $0.result) },
            files: d.files.map { PRDetail.File(path: $0.path, additions: Int($0.additions), deletions: Int($0.deletions)) },
            fetchedAt: date(fromUnix: d.fetchedAt)
        )
    }

    public func tailLogs(after: UInt64, limit: Int) async throws -> LogPage {
        var request = Orchd_V1_TailLogsRequest()
        request.afterSeq = after
        request.limit = Int32(clamping: max(limit, 0))
        let r = try await orchd.tailLogs(request, options: callOptions)
        return LogPage(
            lines: r.entries.map {
                LogLine(seq: $0.seq, at: Date(timeIntervalSince1970: TimeInterval($0.atUnixMs) / 1000),
                        source: $0.source, message: $0.message)
            },
            lastSeq: r.lastSeq, oldestSeq: r.oldestSeq, sources: r.sources
        )
    }

    public func systemMetrics() async throws -> SystemMetrics {
        let r = try await orchd.getSystemMetrics(Orchd_V1_GetSystemMetricsRequest(), options: callOptions)
        return SystemMetrics(
            at: Date(timeIntervalSince1970: TimeInterval(r.atUnixMs) / 1000),
            processes: r.processes.map {
                ProcessStats(name: $0.name, pid: Int($0.pid), rssBytes: $0.rssBytes, cpuPercent: $0.cpuPercent,
                             uptimeSeconds: $0.uptimeSeconds, running: $0.running)
            },
            goroutines: Int(r.goroutines), heapBytes: r.heapBytes,
            data: r.data.map { DataUsage(name: $0.name, path: $0.path, bytes: $0.bytes) },
            tasks: r.tasks.mapValues { Int($0) }
        )
    }

    public func listDocs() async throws -> DocCatalog {
        let r = try await orchd.listDocs(Orchd_V1_ListDocsRequest(), options: callOptions)
        return DocCatalog(
            owners: r.owners.map { o in
                DocOwner(owner: o.owner, refreshSeconds: o.refreshSecs, lastSynced: date(fromUnix: o.lastSynced), status: o.status,
                         repos: o.repos.map {
                             DocRepo(repo: $0.repo, headSHA: $0.headSha, lastSynced: date(fromUnix: $0.lastSynced),
                                     files: Int($0.files), status: $0.status, webURL: $0.webURL, error: $0.error)
                         }, error: o.error)
            },
            files: r.files.map {
                DocFile(sourceURI: $0.sourceUri, owner: $0.owner, repo: $0.repo, path: $0.path,
                        updatedAt: date(fromUnix: $0.updatedAt), webURL: $0.webURL, chunks: Int($0.chunks))
            },
            truncated: r.truncated,
            sync: r.hasSync ? Self.docsSync(r.sync) : nil
        )
    }

    public func syncDocs(owner: String?, force: Bool) async throws -> DocsSyncState {
        var request = Orchd_V1_SyncDocsRequest()
        request.owner = owner ?? ""
        request.force = force
        // Starting the sync restarts orchd's docs unit, which can wait for a
        // tick in progress to stop.
        var options = CallOptions.defaults
        options.timeout = .seconds(40)
        return Self.docsSync(try await orchd.syncDocs(request, options: options))
    }

    public func docsSync() async throws -> DocsSyncState {
        Self.docsSync(try await orchd.getDocsSync(Orchd_V1_GetDocsSyncRequest(), options: callOptions))
    }

    static func docsSync(_ s: Orchd_V1_DocsSyncState) -> DocsSyncState {
        DocsSyncState(running: s.running, owner: s.owner, startedAt: date(fromUnix: s.startedAt),
                      finishedAt: date(fromUnix: s.finishedAt), done: Int(s.done), total: Int(s.total), repo: s.repo)
    }

    public func document(uri: String) async throws -> RebuiltDocument {
        var request = Orchd_V1_GetDocumentRequest()
        request.sourceUri = uri
        // A long doc can take a while to read back and rebuild.
        var options = CallOptions.defaults
        options.timeout = searchTimeout
        let d = try await orchd.getDocument(request, options: options)
        return RebuiltDocument(sourceURI: d.sourceUri, sourceType: d.sourceType, text: d.text, chunks: Int(d.chunks),
                               malformed: d.malformed, oversize: d.oversize, webURL: d.webURL)
    }

    public func registeredSources() async throws -> [RegisteredSource] {
        try await orchd.listRegisteredSources(Orchd_V1_ListRegisteredSourcesRequest(), options: callOptions).sources.map(Self.source)
    }

    public func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource {
        var pb = Orchd_V1_RegisteredSource()
        pb.kind = draft.kind.rawValue
        pb.uri = draft.trimmedURI
        pb.scope = draft.scope.trimmingCharacters(in: .whitespaces)
        pb.tags = draft.tagList
        pb.refreshSecs = draft.refreshSeconds
        var request = Orchd_V1_SaveSourceRequest()
        request.source = pb
        request.replaceID = replacing ?? ""
        return Self.source(try await orchd.saveSource(request, options: callOptions))
    }

    public func removeSource(id: String, purge: Bool) async throws -> PurgeResult {
        var request = Orchd_V1_RemoveSourceRequest()
        request.id = id
        request.purge = purge
        // The user confirmed in the app before this is sent.
        request.confirm = purge
        // Purging lists the whole store, then deletes source by source.
        var options = CallOptions.defaults
        options.timeout = .seconds(60)
        let r = try await orchd.removeSource(request, options: options)
        return PurgeResult(sources: Int(r.purgedSources), chunks: Int(r.purgedChunks))
    }

    public func refreshSource(id: String?) async throws {
        var request = Orchd_V1_RefreshSourceRequest()
        request.id = id ?? ""
        // Restarting the scheduler can wait out a refresh in progress.
        var options = CallOptions.defaults
        options.timeout = .seconds(40)
        _ = try await orchd.refreshSource(request, options: options)
    }

    func orchdSettings() async throws -> Orchd_V1_ListSettingsResponse {
        try await orchd.listSettings(Orchd_V1_ListSettingsRequest(), options: callOptions)
    }

    // Changes read the file, write it and re-read it; still well under the
    // longer bound used for other writes.
    private var settingsWriteOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = .seconds(15)
        return options
    }

    func orchdSetSetting(_ request: Orchd_V1_SetSettingEnabledRequest) async throws -> Orchd_V1_SettingItem {
        try await orchd.setSettingEnabled(request, options: settingsWriteOptions)
    }

    func orchdAddSetting(_ request: Orchd_V1_AddSettingRequest) async throws -> Orchd_V1_SettingItem {
        try await orchd.addSetting(request, options: settingsWriteOptions)
    }

    func orchdRemoveSetting(_ request: Orchd_V1_RemoveSettingRequest) async throws {
        _ = try await orchd.removeSetting(request, options: settingsWriteOptions)
    }

    func getSafetyNet() async throws -> Orchd_V1_SafetyNetState {
        try await orchd.getSafetyNet(Orchd_V1_GetSafetyNetRequest(), options: callOptions)
    }

    func runSafetyNetInstall(rulesOnly: Bool) async throws -> Orchd_V1_SafetyNetState {
        var request = Orchd_V1_InstallSafetyNetRequest()
        request.rulesOnly = rulesOnly
        // An install clones the plugin repository and runs several claude
        // commands, each bounded by orchd; this covers all of them.
        var options = CallOptions.defaults
        options.timeout = .seconds(600)
        return try await orchd.installSafetyNet(request, options: options)
    }

    static func source(_ s: Orchd_V1_RegisteredSource) -> RegisteredSource {
        RegisteredSource(id: s.id, kind: SourceKind(rawValue: s.kind), uri: s.uri, scope: s.scope, tags: s.tags,
                         refreshSeconds: s.refreshSecs, lastFetched: date(fromUnix: s.lastFetched), status: s.status,
                         webURL: s.webURL, searchSourceURI: s.searchSourceUri, searchAllTags: s.searchAllTags)
    }

    static func scope(_ s: Orchd_V1_GitHubScope) -> GitHubScope {
        GitHubScope(owner: s.owner, isUser: s.kind == "user", enabled: s.enabled,
                    items: GitHubItemsMode(rawValue: s.items) ?? .involving,
                    repoMode: GitHubRepoMode(rawValue: s.repoMode) ?? .all, repos: s.repos)
    }

    private var callOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = callTimeout
        return options
    }
}
