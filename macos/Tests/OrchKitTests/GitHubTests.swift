import Foundation
import Testing
@testable import OrchKit

@Suite struct GitHubURLTests {
    @Test(arguments: [
        ("https://github.com/o/r/pull/1", true),
        ("https://github.com/o/r/issues/12", true),
        ("https://github.com/o/r/discussions/1", false),
        ("https://github.com/o/r/issues/1#issuecomment-2", false),
        ("https://github.com:444/o/r/issues/1", false),
        ("http://github.com/o/r/issues/1", false),
        ("https://evil.example/o/r/issues/1", false),
        ("file:///o/r/issues/1", false),
    ])
    func itemURL(raw: String, allowed: Bool) {
        #expect((githubItemURL(raw) != nil) == allowed)
    }

    @Test func prLinksStayPullOnly() {
        #expect(PullRequest(url: "https://github.com/o/r/issues/1", title: "", repo: "").webURL == nil)
        #expect(PullRequest(url: "https://github.com/o/r/pull/1", title: "", repo: "").webURL != nil)
    }

    @Test func coverageDescribesTheScope() {
        #expect(GitHubScope(owner: "o", isUser: false, enabled: true).coverage == "Involving me · all repos")
        #expect(GitHubScope(owner: "o", isUser: false, items: .all, repoMode: .include, repos: ["a"]).coverage == "Everything open · 1 repo")
        #expect(GitHubScope(owner: "o", isUser: false, repoMode: .include, repos: ["a", "b"]).coverage == "Involving me · 2 repos")
        #expect(GitHubScope(owner: "o", isUser: false, repoMode: .exclude, repos: ["a"]).coverage == "Involving me · all but 1 repo")
        #expect(GitHubScope(owner: "o", isUser: false, repoMode: .exclude, repos: ["a", "b"]).coverage == "Involving me · all but 2 repos")
        #expect(GitHubScope(owner: "o", isUser: false, everythingRepos: ["a"]).coverage == "Involving me + everything in 1 repo · all repos")
        #expect(GitHubScope(owner: "o", isUser: false, everythingRepos: ["a", "b"]).coverage == "Involving me + everything in 2 repos · all repos")
        #expect(GitHubScope(owner: "o", isUser: false, items: .all, everythingRepos: ["a"]).coverage == "Everything open · all repos", "the list adds nothing to Everything open")
    }

    @Test func labelsAndOwners() {
        #expect(Set(GitHubReason.allCases.map(\.label)).count == GitHubReason.allCases.count)
        #expect(Set(GitHubItemsMode.allCases.map(\.label)).count == 2)
        #expect(Set(GitHubRepoMode.allCases.map(\.label)).count == 3)
        #expect(GitHubItem(url: "u", title: "t", repo: "WippData/api").owner == "WippData")
        #expect(GitHubTeam(org: "o", slug: "s", name: "n").id == "o/s")
        #expect(GitHubRepo(name: "API").id == "api")
    }
}

private struct Boom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private final class FakeGitHub: GitHubAPI, @unchecked Sendable {
    var scopes = [
        GitHubScope(owner: "willesq", isUser: true, enabled: true),
        GitHubScope(owner: "WippData", isUser: false),
    ]
    var warning: String?
    var items = GitHubItemList(items: [], updatedAt: nil)
    var failScopes = false
    var failSet = false
    var failItems = false
    var failRepos = false
    var newRepos: [String] = []
    var failSweep = false
    /// What the cache holds once a requested sweep has run; nil = it never does.
    var afterSweep: GitHubItemList?
    private(set) var sweepRequests = 0
    private(set) var itemLoadsAtSweep = 0
    /// When set, a save waits here until the test opens it.
    var setGate: Gate?
    private(set) var sets: [GitHubScope] = []
    private(set) var repoLoads: [String] = []
    private(set) var itemLoads = 0
    private(set) var refreshes: [Bool] = []

    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity {
        GitHubIdentity(login: "willesq", orgs: ["WippData"], teams: [GitHubTeam(org: "WippData", slug: "admins", name: "Admins")])
    }

    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList {
        refreshes.append(refresh)
        if failScopes { throw Boom() }
        return GitHubScopeList(scopes: scopes, warning: warning)
    }

    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope {
        sets.append(scope)
        await setGate?.wait()
        if failSet { throw Boom() }
        return scope
    }

    func gitHubRepos(owner: String) async throws -> [GitHubRepo] {
        repoLoads.append(owner)
        if failRepos { throw Boom() }
        return [GitHubRepo(name: "api", isPrivate: true), GitHubRepo(name: "web")] + newRepos.map { GitHubRepo(name: $0) }
    }

    func gitHubItems() async throws -> GitHubItemList {
        itemLoads += 1
        if failItems { throw Boom() }
        return items
    }

    func requestGitHubSweep() async throws {
        sweepRequests += 1
        itemLoadsAtSweep = itemLoads
        if failSweep { throw Boom() }
        if let afterSweep { items = afterSweep }
    }
}

private func item(_ n: Int, pr: Bool = true, attention: Attention? = nil, ago: TimeInterval) -> GitHubItem {
    GitHubItem(url: "https://github.com/o/r/\(pr ? "pull" : "issues")/\(n)", title: "t\(n)", repo: "o/r", isPR: pr,
               number: n, updatedAt: Date(timeIntervalSinceNow: -ago), attention: attention)
}

@MainActor
@Suite struct GitHubModelTests {
    @Test func refreshLoadsEverything() async {
        let api = FakeGitHub()
        api.warning = "Could not reach GitHub"
        let model = GitHubModel(api: api)
        await model.refresh(rediscover: true)
        #expect(model.scopes.count == 2 && model.identity?.login == "willesq" && model.identity?.teams.count == 1)
        #expect(model.warning == "Could not reach GitHub" && model.items != nil && api.refreshes == [true])
    }

    @Test func checkFollowsTheRequestedSweepUntilTheCacheMoves() async {
        let old = Date(timeIntervalSince1970: 1_800_000_000)
        let api = FakeGitHub()
        api.items = GitHubItemList(items: [], updatedAt: old)
        api.afterSweep = GitHubItemList(items: [item(1, ago: 0)], updatedAt: old.addingTimeInterval(1))
        let model = GitHubModel(api: api, checkPoll: .milliseconds(1), checkTimeout: .seconds(30))
        await model.checkGitHub(rediscover: true)
        #expect(api.sweepRequests == 1 && api.refreshes == [true])
        #expect(api.itemLoadsAtSweep > 0, "the time to compare against comes from before the request")
        #expect(model.items?.items.count == 1 && model.checkWarning == nil && !model.isCheckingGitHub)
    }

    @Test func checkCountsAFirstEverSweepAsDone() async {
        let api = FakeGitHub()
        api.afterSweep = GitHubItemList(items: [], updatedAt: Date())
        let model = GitHubModel(api: api, checkPoll: .milliseconds(1), checkTimeout: .seconds(30))
        await model.checkGitHub()
        #expect(model.checkWarning == nil)
    }

    @Test func aSweepThatNeverLandsEndsInAWarningAndKeepsTheItems() async {
        let api = FakeGitHub()
        api.items = GitHubItemList(items: [item(1, ago: 0)], updatedAt: Date())
        let model = GitHubModel(api: api, checkPoll: .milliseconds(1), checkTimeout: .milliseconds(20))
        await model.checkGitHub()
        #expect(model.checkWarning?.contains("may be out of date") == true)
        #expect(model.items?.items.count == 1 && !model.isCheckingGitHub)
        api.afterSweep = GitHubItemList(items: [], updatedAt: Date().addingTimeInterval(5))
        await model.checkGitHub()
        #expect(model.checkWarning == nil, "the next check clears it")
    }

    @Test func aRefusedSweepRequestIsReportedWithoutWaiting() async {
        let api = FakeGitHub()
        api.failSweep = true
        let model = GitHubModel(api: api, checkPoll: .seconds(60), checkTimeout: .seconds(60))
        await model.checkGitHub()
        #expect(model.checkWarning == "Could not ask orchd to check GitHub: unavailable" && !model.isCheckingGitHub)
    }

    @Test func failuresAreReported() async {
        let api = FakeGitHub()
        api.failScopes = true
        let model = GitHubModel(api: api)
        await model.refresh()
        #expect(model.scopesError == "unavailable" && model.lastError == nil, "loaded items must not hide the scopes error")
        #expect(model.needsScopes)
        api.failScopes = false
        api.failItems = true
        await model.refresh()
        #expect(model.scopesError == nil && model.lastError == "unavailable" && model.scopes.count == 2)
        #expect(!model.needsScopes)
    }

    @Test func aCancelledRefreshIsNotAnError() async {
        // Leaving the tab cancels the poll mid-request; the transport then
        // fails, and that failure is the cancellation, not orchd's answer.
        let api = FakeGitHub()
        api.failScopes = true
        api.failItems = true
        let model = GitHubModel(api: api)
        await Task { @MainActor in
            withUnsafeCurrentTask { $0?.cancel() }
            await model.refresh()
        }.value
        #expect(model.scopesError == nil && model.lastError == nil)
        #expect(!model.isRefreshing, "a later refresh must not be skipped")
        await model.refresh()
        #expect(model.scopesError == "unavailable" && model.lastError == "unavailable")
    }

    @Test func pollingRetriesScopesUntilTheyLoad() async throws {
        let api = FakeGitHub()
        api.failScopes = true
        let model = GitHubModel(api: api, interval: .milliseconds(20))
        model.start()
        try await eventually { model.scopesError != nil }
        #expect(model.scopes.isEmpty && model.scopesError != nil)
        api.failScopes = false
        // Reason: the loaded refresh goes on to load items; stopped before it
        // ends, it is still running when start() refreshes, which then skips
        // (refreshDoesNotOverlap) and leaves one refresh uncounted (SP-082).
        try await eventually { model.scopes.count == 2 && model.scopesError == nil && !model.isRefreshing }
        model.stop()
        #expect(model.scopes.count == 2 && model.identity?.login == "willesq" && model.scopesError == nil)
        let loads = api.refreshes.count
        let items = api.itemLoads
        model.start()
        try await eventually { api.itemLoads >= items + 2 }
        model.stop()
        #expect(api.refreshes.count == loads + 1, "once loaded, polling reloads only items")
    }

    @Test func togglingAndItemsModeAreSent() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        await model.refresh()
        await model.setEnabled("wippdata", true)
        await model.setItems("WippData", .all)
        #expect(api.sets.map(\.enabled) == [true, true] && api.sets.last?.items == .all)
        #expect(model.scopes[1].enabled && model.scopes[1].items == .all && model.saving.isEmpty)
        await model.setEnabled("WippData", true)
        #expect(api.sets.count == 2, "an unchanged scope is not sent")
        await model.setEnabled("nobody", true)
        #expect(api.sets.count == 2)
    }

    @Test func repoPicksFallBackToAllWhenEmpty() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        await model.refresh()
        await model.setRepos("WippData", mode: .include, repos: ["web", "api", "web"])
        #expect(model.scopes[1].repoMode == .include && model.scopes[1].repos == ["api", "web"])
        await model.setRepos("WippData", mode: .exclude, repos: [])
        #expect(model.scopes[1].repoMode == .all && model.scopes[1].repos.isEmpty)
    }

    @Test func everythingReposAreSortedSentAndCleared() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        await model.refresh()
        await model.setEverythingRepos("WippData", repos: ["web", "api", "web"])
        #expect(model.scopes[1].everythingRepos == ["api", "web"] && api.sets.last?.everythingRepos == ["api", "web"])
        await model.setItems("WippData", .all)
        #expect(api.sets.last?.everythingRepos == ["api", "web"], "switching to Everything open keeps the list")
        await model.setEverythingRepos("WippData", repos: [])
        #expect(model.scopes[1].everythingRepos.isEmpty)
    }

    @Test func failedChangeKeepsTheScopeAndSaysSo() async {
        let api = FakeGitHub()
        api.failSet = true
        let model = GitHubModel(api: api)
        await model.refresh()
        await model.setEnabled("WippData", true)
        #expect(!model.scopes[1].enabled && model.lastError == "Could not update WippData: unavailable")
    }

    @Test func aSecondChangeWhileSavingIsIgnored() async {
        let api = FakeGitHub()
        let gate = Gate()
        defer { gate.open() }
        api.setGate = gate
        let model = GitHubModel(api: api)
        await model.refresh()
        let first = Task { await model.setEnabled("WippData", true) }
        try? await eventually { model.saving.contains("WippData") }
        #expect(model.saving.contains("WippData"))
        _ = await model.setItems("WippData", .all)
        gate.open()
        _ = await first.value
        #expect(api.sets.count == 1)
    }

    @Test func reposAreKeptPerOwner() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        #expect(model.reposFor("WippData") == nil)
        await model.loadRepos("WippData")
        #expect(model.reposFor("WIPPDATA")?.count == 2)
        api.failRepos = true
        await model.loadRepos("other")
        #expect(model.repoError("other") == "unavailable" && model.reposFor("other") == nil)
    }

    @Test func aRepoMadeSinceTheLastLoadShowsUp() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        await model.loadRepos("WippData")
        api.newRepos = ["brand-new"]
        await model.loadRepos("wippdata")
        #expect(api.repoLoads == ["WippData", "wippdata"])
        #expect(model.reposFor("WippData")?.map(\.name) == ["api", "web", "brand-new"])
    }

    @Test func aFailedReloadKeepsTheListAndSaysSo() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        await model.loadRepos("WippData")
        api.failRepos = true
        await model.loadRepos("WippData")
        #expect(model.reposFor("WippData")?.count == 2 && model.repoError("WippData") == "unavailable")
        api.failRepos = false
        await model.loadRepos("WippData")
        #expect(model.repoError("WippData") == nil)
    }

    @Test func visibleItemsFilterAndOrder() async {
        let api = FakeGitHub()
        api.items = GitHubItemList(items: [
            item(1, ago: 300), item(2, pr: false, ago: 10), item(3, attention: .ciFailed, ago: 900), item(4, ago: 60),
        ], updatedAt: Date())
        let model = GitHubModel(api: api)
        await model.refresh()
        #expect(model.visibleItems.map(\.number) == [3, 2, 4, 1])
        model.filter = .prs
        #expect(model.visibleItems.map(\.number) == [3, 4, 1])
        model.filter = .issues
        #expect(model.visibleItems.map(\.number) == [2])
    }

    @Test func pollingRefreshesItemsAndStops() async throws {
        let api = FakeGitHub()
        let model = GitHubModel(api: api, interval: .milliseconds(20))
        model.start()
        model.start()
        try await eventually { api.itemLoads >= 3 }
        model.stop()
        let loads = api.itemLoads
        #expect(loads >= 3)
        try await Task.sleep(for: .milliseconds(80))
        #expect(api.itemLoads <= loads + 1)
    }
}

@Suite struct GitHubURLHardeningTests {
    @Test(arguments: [
        "https://github.com/a%2Fb/pull/1",
        "https://github.com/../x/pull/1",
        "https://github.com/./x/issues/1",
        "https://github.com/o/.hidden/pull/1",
    ])
    func rejected(raw: String) {
        #expect(githubItemURL(raw) == nil)
    }

    @Test func issueInARepoNamedPullIsNotAPR() {
        #expect(PullRequest(url: "https://github.com/o/pull/issues/1", title: "", repo: "").webURL == nil)
        #expect(PullRequest(url: "https://github.com/pull/r/issues/1", title: "", repo: "").webURL == nil)
        #expect(PullRequest(url: "https://github.com/o/pull/pull/1", title: "", repo: "").webURL != nil)
    }
}

@MainActor
@Suite struct GitHubModelRobustnessTests {
    @Test func itemsErrorClearsOnSuccess() async {
        let api = FakeGitHubForRobustness()
        api.failItems = true
        let model = GitHubModel(api: api)
        await model.refreshItems()
        #expect(model.lastError == "unavailable")
        api.failItems = false
        await model.refreshItems()
        #expect(model.lastError == nil)
    }

    @Test func repoLoadFailureIsPerOwnerAndRetries() async {
        let api = FakeGitHubForRobustness()
        api.failRepos = true
        let model = GitHubModel(api: api)
        await model.loadRepos("WippData")
        #expect(model.repoError("wippdata") == "unavailable" && model.reposFor("WippData") == nil && model.lastError == nil)
        api.failRepos = false
        await model.loadRepos("WippData")
        #expect(model.repoError("WippData") == nil && model.reposFor("WippData")?.count == 1)
    }

    @Test func changesReportWhetherTheySaved() async {
        let api = FakeGitHubForRobustness()
        let model = GitHubModel(api: api)
        await model.refresh()
        #expect(await model.setEnabled("WippData", true))
        #expect(await model.setEnabled("WippData", true), "an unchanged scope counts as stored")
        #expect(!(await model.setEnabled("nobody", true)))
        api.failSet = true
        #expect(!(await model.setItems("WippData", .all)))
    }

    @Test func aReloadDuringASaveKeepsTheLocalChange() async {
        let api = FakeGitHubForRobustness()
        let gate = Gate()
        defer { gate.open() }
        api.setGate = gate
        let model = GitHubModel(api: api)
        await model.refresh()
        let save = Task { await model.setEnabled("WippData", true) }
        try? await eventually { model.saving.contains("WippData") }
        // The server still has the old value; the reload must not show it.
        await model.refresh()
        #expect(model.saving.contains("WippData"))
        gate.open()
        _ = await save.value
        #expect(model.scopes[1].enabled)
    }

    @Test func refreshDoesNotOverlap() async {
        let api = FakeGitHubForRobustness()
        let gate = Gate()
        defer { gate.open() }
        api.scopesGate = gate
        let model = GitHubModel(api: api)
        let first = Task { await model.refresh() }
        try? await eventually { model.isRefreshing }
        #expect(model.isRefreshing)
        await model.refresh()
        gate.open()
        await first.value
        #expect(api.scopeLoads == 1 && !model.isRefreshing)
    }
}

private final class FakeGitHubForRobustness: GitHubAPI, @unchecked Sendable {
    var failItems = false, failRepos = false, failSet = false
    /// When set, a save or a scopes load waits here until the test opens it.
    var setGate: Gate?, scopesGate: Gate?
    private(set) var scopeLoads = 0

    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity { GitHubIdentity(login: "willesq") }

    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList {
        scopeLoads += 1
        await scopesGate?.wait()
        return GitHubScopeList(scopes: [
            GitHubScope(owner: "willesq", isUser: true, enabled: true),
            GitHubScope(owner: "WippData", isUser: false),
        ])
    }

    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope {
        await setGate?.wait()
        if failSet { throw RobustnessBoom() }
        return scope
    }

    func gitHubRepos(owner: String) async throws -> [GitHubRepo] {
        if failRepos { throw RobustnessBoom() }
        return [GitHubRepo(name: "api")]
    }

    func gitHubItems() async throws -> GitHubItemList {
        if failItems { throw RobustnessBoom() }
        return GitHubItemList(items: [], updatedAt: nil)
    }

    func requestGitHubSweep() async throws {}
}

private struct RobustnessBoom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }
