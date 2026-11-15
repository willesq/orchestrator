import Foundation
import Testing
@testable import OrchKit

@Suite struct DaemonClientTests {
    @Test func missingSocketFailsFast() async throws {
        let dir = try TempDir()
        let client = try DaemonClient(socketPath: dir.url.appendingPathComponent("orchd.sock").path, callTimeout: .seconds(1))
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let started = ContinuousClock.now
        await #expect(throws: (any Error).self) { try await client.status() }
        #expect(ContinuousClock.now - started < .seconds(5))
    }
}

/// Runs against a real orchd only when ORCH_LIVE_SOCKET names its socket, e.g.
/// `ORCH_LIVE_SOCKET=~/.local/share/orch/orchd.sock swift test`.
@Suite(.enabled(if: ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"] != nil))
struct LiveDaemonTests {
    @Test func statusFromRunningDaemon() async throws {
        let socket = ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"]!
        let client = try DaemonClient(socketPath: socket)
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let status = try await client.status()
        #expect(!status.version.isEmpty)
        #expect(status.subsystems.contains { $0.name == "pr-babysitter" })
    }

    @Test func searchAndSourcesFromRunningDaemon() async throws {
        let socket = ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"]!
        let client = try DaemonClient(socketPath: socket)
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let result = try await client.search(SearchQuery(text: "orchestrator daemon", limit: 3))
        #expect(!result.hits.isEmpty && result.hits.count <= 3)
        let sources = try await client.listSources(type: "", limit: 5)
        #expect(sources.sources.count <= 5 && sources.totalSources >= sources.sources.count)
    }

    @Test func tasksAndRoutingFromRunningDaemon() async throws {
        let socket = ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"]!
        let client = try DaemonClient(socketPath: socket)
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let tasks = try await client.listTasks(status: .all, limit: 5)
        #expect(tasks.count <= 5)
        _ = try await client.recommend(taskKind: "code")
    }
}

@Suite struct DaemonClientRoundTripTests {
    @Test func mapsStatusResponse() async throws {
        var response = Orchd_V1_StatusResponse()
        response.version = "0.18.0"
        response.uptimeSeconds = 90
        var unit = Orchd_V1_SubsystemStatus()
        unit.name = "pr-babysitter"
        unit.state = "failed"
        unit.detail = "open-PR polling via gh"
        unit.error = "gh: not logged in"
        unit.restarts = 2
        response.subsystems = [unit]
        response.spaces.state = "off"

        try await withFakeOrchd(FakeOrchdService(status: response)) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let status = try await client.status()
            #expect(status == DaemonStatus(version: "0.18.0", uptimeSeconds: 90, subsystems: [
                Subsystem(name: "pr-babysitter", state: "failed", detail: "open-PR polling via gh",
                          error: "gh: not logged in", restarts: 2),
            ], spacesState: "off"))
            #expect(status.spacesOff)
            #expect(Health(status: status) == .degraded(status, failed: ["pr-babysitter"]))
        }
    }
}

@Suite struct ListPRsRoundTripTests {
    @Test func mapsPullRequestsAndCacheTime() async throws {
        var failing = Orchd_V1_PullRequest()
        failing.url = "https://github.com/o/r/pull/1"
        failing.title = "fix"
        failing.repo = "o/r"
        failing.ciState = "failure"
        failing.reviewDecision = "APPROVED"
        failing.mergeable = "MERGEABLE"
        failing.reasons = ["authored"]
        failing.attention = "ci_failed"
        var quiet = Orchd_V1_PullRequest()
        quiet.url = "https://github.com/o/r/pull/2"
        quiet.isDraft = true
        quiet.attention = ""
        var future = Orchd_V1_PullRequest()
        future.url = "https://github.com/o/r/pull/3"
        future.attention = "some_new_reason"
        var response = Orchd_V1_ListPRsResponse()
        response.prs = [failing, quiet, future]
        response.updatedAt = 1_800_000_000

        try await withFakeOrchd(FakeOrchdService(prs: response)) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let list = try await client.listPRs()
            #expect(list.updatedAt == Date(timeIntervalSince1970: 1_800_000_000))
            #expect(list.prs.first == PullRequest(
                url: "https://github.com/o/r/pull/1", title: "fix", repo: "o/r", ciState: "failure",
                reviewDecision: "APPROVED", mergeable: "MERGEABLE", reasons: ["authored"], attention: .ciFailed
            ))
            #expect(list.prs[1].isDraft && list.prs[1].attention == nil)
            #expect(list.prs[2].attention == nil, "an unknown reason must not crash or be guessed")
            #expect(list.needsYou.map(\.url) == ["https://github.com/o/r/pull/1"])
        }
    }

    @Test func neverWrittenCacheHasNoTime() async throws {
        try await withFakeOrchd(FakeOrchdService()) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            #expect(try await client.listPRs() == PRList(prs: [], updatedAt: nil))
        }
    }
}

@Suite struct MemoryRoundTripTests {
    private func withClient(_ service: FakeOrchdService, _ body: (DaemonClient) async throws -> Void) async throws {
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            try await body(client)
        }
    }

    @Test func searchSendsNormalizedQueryAndMapsHits() async throws {
        var hit = Orchd_V1_SearchHit()
        hit.text = "chunk"
        hit.sourceUri = "repo://o/r/README.md"
        hit.sourceType = "repo_doc"
        hit.scope = "o/r"
        hit.score = 0.5
        hit.keywordScore = 3
        hit.exact = true
        hit.tags = ["repo:r"]
        hit.startedAt = 1_700_000_000
        var service = FakeOrchdService()
        service.search.hits = [hit]
        service.search.fallback = "keyword index missing"
        try await withClient(service) { client in
            let result = try await client.search(SearchQuery(text: " q ", limit: 0, sourceType: "repo_doc", mode: .hybrid))
            #expect(result.fallback == "keyword index missing")
            #expect(result.hits == [SearchHit(
                text: "chunk", sourceURI: "repo://o/r/README.md", sourceType: "repo_doc", scope: "o/r", score: 0.5,
                keywordScore: 3, exact: true, tags: ["repo:r"], startedAt: Date(timeIntervalSince1970: 1_700_000_000)
            )])
            let sent = try #require(service.recorder.last(Orchd_V1_SearchRequest.self))
            #expect(sent.query == "q" && sent.topK == 1 && sent.sourceType == "repo_doc" && sent.mode == "hybrid")
        }
    }

    @Test func searchWithoutFallbackHasNone() async throws {
        try await withClient(FakeOrchdService()) { client in
            let result = try await client.search(SearchQuery(text: "q"))
            #expect(result.fallback == nil)
        }
    }

    @Test func listSourcesMapsTotals() async throws {
        var source = Orchd_V1_SourceSummary()
        source.sourceUri = "session://1"
        source.sourceType = "session"
        source.chunks = 3
        source.ingestedAt = 1_700_000_000
        var service = FakeOrchdService()
        service.sources.sources = [source]
        service.sources.totalSources = 7
        service.sources.totalChunks = 40
        try await withClient(service) { client in
            let list = try await client.listSources(type: "session", limit: 5)
            #expect(list == SourceList(
                sources: [SourceSummary(uri: "session://1", sourceType: "session", chunks: 3,
                                        ingestedAt: Date(timeIntervalSince1970: 1_700_000_000))],
                totalSources: 7, totalChunks: 40
            ))
            let sent = try #require(service.recorder.last(Orchd_V1_ListSourcesRequest.self))
            #expect(sent.sourceType == "session" && sent.limit == 5)
        }
    }

    @Test func restartOneAllAndUnknown() async throws {
        try await withClient(FakeOrchdService()) { client in
            let one = try await client.restart("qdrant")
            let all = try await client.restart(nil)
            #expect(one == ["qdrant"])
            #expect(all == ["qdrant", "handoff"])
            await #expect(throws: (any Error).self) { try await client.restart("nope") }
        }
    }
}

@Suite struct TasksRoundTripTests {
    private func withClient(_ service: FakeOrchdService, _ body: (DaemonClient) async throws -> Void) async throws {
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            try await body(client)
        }
    }

    private func pbTask() -> Orchd_V1_Task {
        var route = Orchd_V1_Route()
        route.taskKind = "code"
        route.provider = "claude-code"
        route.model = "opus"
        route.evidenceN = 4
        route.confidence = "medium"
        var t = Orchd_V1_Task()
        t.id = "task-1"
        t.agent = "dev"
        t.body = "fix"
        t.result = "done"
        t.status = "complete"
        t.updatedAt = "2026-10-23T10:00:00Z"
        t.costUsd = 1.25
        t.durationMs = 90_000
        t.turns = 7
        t.sessionID = "s1"
        t.models = ["opus"]
        t.prs = ["https://github.com/o/r/pull/1"]
        t.verification = "passed: go test"
        t.route = route
        t.rating = "good"
        t.prOutcome = "good"
        return t
    }

    @Test func listAndGetMapEveryField() async throws {
        var service = FakeOrchdService()
        service.tasks.tasks = [pbTask()]
        try await withClient(service) { client in
            let list = try await client.listTasks(status: .complete, limit: 20)
            let expected = TaskRecord(
                id: "task-1", agent: "dev", body: "fix", result: "done", status: "complete",
                updatedAt: "2026-10-23T10:00:00Z", costUSD: 1.25, durationMS: 90_000, turns: 7, sessionID: "s1",
                models: ["opus"], prs: ["https://github.com/o/r/pull/1"], verification: "passed: go test",
                route: RouteInfo(taskKind: "code", provider: "claude-code", model: "opus", evidenceCount: 4, confidence: "medium"),
                rating: .good, prOutcome: "good"
            )
            #expect(list == [expected])
            let sent = try #require(service.recorder.last(Orchd_V1_ListTasksRequest.self))
            #expect(sent.status == "complete" && sent.limit == 20)
            let found = try await client.task(id: "task-1")
            let missing = try await client.task(id: "nope")
            #expect(found == expected && missing == nil)
        }
    }

    @Test func unroutedTaskHasNoRoute() async throws {
        var service = FakeOrchdService()
        var t = Orchd_V1_Task()
        t.id = "x"
        t.rating = ""
        service.tasks.tasks = [t]
        try await withClient(service) { client in
            let list = try await client.listTasks(status: .all, limit: 1)
            #expect(list.first?.route == nil && list.first?.rating == nil)
        }
    }

    @Test func rateDispatchAndRecommend() async throws {
        var service = FakeOrchdService()
        service.dispatch.instanceID = "task-9"
        service.dispatch.workerEnabled = true
        var route = Orchd_V1_Route()
        route.model = "sonnet"
        service.recommend.route = route
        service.recommend.fromTable = false
        try await withClient(service) { client in
            try await client.rate(id: "task-1", .rework)
            let rated = try #require(service.recorder.last(Orchd_V1_RateTaskRequest.self))
            #expect(rated.id == "task-1" && rated.rating == "rework")

            let outcome = try await client.dispatch(DispatchDraft(agent: "dev", body: "b", cwd: "/r", taskKind: "code", model: "m"))
            #expect(outcome == DispatchOutcome(id: "task-9", workerEnabled: true, route: nil))
            let sent = try #require(service.recorder.last(Orchd_V1_DispatchRequest.self))
            #expect(sent.agent == "dev" && sent.body == "b" && sent.cwd == "/r" && sent.taskKind == "code" && sent.model == "m")

            let rec = try await client.recommend(taskKind: "review")
            #expect(rec.route == RouteInfo(model: "sonnet") && rec.fromTable == false)
        }
    }
}

@Suite struct GitHubRoundTripTests {
    private func withClient(_ service: FakeOrchdService, _ body: (DaemonClient) async throws -> Void) async throws {
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            try await body(client)
        }
    }

    @Test func identityScopesReposAndItems() async throws {
        var service = FakeOrchdService()
        service.ghIdentity.login = "willesq"
        service.ghIdentity.orgs = ["WippData"]
        var team = Orchd_V1_GitHubTeam()
        team.org = "WippData"
        team.slug = "admins"
        team.name = "Admins"
        service.ghIdentity.teams = [team]
        var me = Orchd_V1_GitHubScope()
        me.owner = "willesq"
        me.kind = "user"
        me.enabled = true
        me.items = "involving"
        me.repoMode = "all"
        var org = Orchd_V1_GitHubScope()
        org.owner = "WippData"
        org.kind = "org"
        org.items = "bogus"
        org.repoMode = "exclude"
        org.repos = ["legacy"]
        service.ghScopes.scopes = [me, org]
        service.ghScopes.warning = "Could not reach GitHub"
        var repo = Orchd_V1_GitHubRepo()
        repo.name = "api"
        repo.`private` = true
        repo.archived = true
        service.ghRepos.repos = [repo]
        var pr = Orchd_V1_GitHubItem()
        pr.url = "https://github.com/WippData/api/pull/3"
        pr.title = "fix"
        pr.repo = "WippData/api"
        pr.kind = "pr"
        pr.number = 3
        pr.author = "willesq"
        pr.updatedAt = "2026-10-23T10:00:00Z"
        pr.ciState = "failure"
        pr.reasons = ["authored", "something_new"]
        pr.attention = "ci_failed"
        var issue = Orchd_V1_GitHubItem()
        issue.url = "https://github.com/willesq/orch/issues/9"
        issue.kind = "issue"
        issue.updatedAt = "not a date"
        service.ghItems.items = [pr, issue]
        service.ghItems.updatedAt = 1_800_000_000

        try await withClient(service) { client in
            let id = try await client.gitHubIdentity(refresh: true)
            #expect(id == GitHubIdentity(login: "willesq", orgs: ["WippData"], teams: [GitHubTeam(org: "WippData", slug: "admins", name: "Admins")]))
            #expect(service.recorder.last(Orchd_V1_GetGitHubIdentityRequest.self)?.refresh == true)

            let list = try await client.gitHubScopes(refresh: false)
            #expect(list.warning == "Could not reach GitHub")
            #expect(list.scopes == [
                GitHubScope(owner: "willesq", isUser: true, enabled: true),
                GitHubScope(owner: "WippData", isUser: false, items: .involving, repoMode: .exclude, repos: ["legacy"]),
            ])

            let repos = try await client.gitHubRepos(owner: "WippData")
            #expect(repos == [GitHubRepo(name: "api", isPrivate: true, isArchived: true)])
            #expect(service.recorder.last(Orchd_V1_ListGitHubReposRequest.self)?.owner == "WippData")

            let items = try await client.gitHubItems()
            #expect(items.updatedAt == Date(timeIntervalSince1970: 1_800_000_000))
            #expect(items.items[0].isPR && items.items[0].number == 3 && items.items[0].reasons == [.authored])
            #expect(items.items[0].attention == .ciFailed && items.items[0].updatedAt != nil)
            #expect(!items.items[1].isPR && items.items[1].updatedAt == nil)
        }
    }

    @Test func requestsAGitHubSweep() async throws {
        let service = FakeOrchdService()
        try await withClient(service) { client in
            try await client.requestGitHubSweep()
            #expect(service.recorder.last(Orchd_V1_RequestGitHubSweepRequest.self) != nil)
        }
    }

    @Test func setScopeSendsReposOnlyWhenPicked() async throws {
        let service = FakeOrchdService()
        try await withClient(service) { client in
            let stored = try await client.setGitHubScope(
                GitHubScope(owner: "WippData", isUser: false, enabled: true, items: .all, repoMode: .include, repos: ["api"]))
            #expect(stored == GitHubScope(owner: "WippData", isUser: false, enabled: true, items: .all, repoMode: .include, repos: ["api"]))
            let sent = try #require(service.recorder.last(Orchd_V1_SetGitHubScopeRequest.self))
            #expect(sent.scope.owner == "WippData" && sent.scope.items == "all" && sent.scope.repoMode == "include" && sent.scope.repos == ["api"])
            _ = try await client.setGitHubScope(GitHubScope(owner: "WippData", isUser: false, repoMode: .all, repos: ["stale"]))
            #expect(service.recorder.last(Orchd_V1_SetGitHubScopeRequest.self)?.scope.repos.isEmpty == true)
        }
    }
}
