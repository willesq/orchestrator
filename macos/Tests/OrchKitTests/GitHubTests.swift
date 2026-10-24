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
    var setDelay: Duration = .zero
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
        try? await Task.sleep(for: setDelay)
        if failSet { throw Boom() }
        return scope
    }

    func gitHubRepos(owner: String) async throws -> [GitHubRepo] {
        repoLoads.append(owner)
        if failRepos { throw Boom() }
        return [GitHubRepo(name: "api", isPrivate: true), GitHubRepo(name: "web")]
    }

    func gitHubItems() async throws -> GitHubItemList {
        itemLoads += 1
        if failItems { throw Boom() }
        return items
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

    @Test func failuresAreReported() async {
        let api = FakeGitHub()
        api.failScopes = true
        let model = GitHubModel(api: api)
        await model.refresh()
        #expect(model.lastError == "unavailable")
        api.failScopes = false
        api.failItems = true
        await model.refresh()
        #expect(model.lastError == "unavailable" && model.scopes.count == 2)
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
        api.setDelay = .milliseconds(200)
        let model = GitHubModel(api: api)
        await model.refresh()
        let first = Task { await model.setEnabled("WippData", true) }
        try? await Task.sleep(for: .milliseconds(50))
        #expect(model.saving.contains("WippData"))
        _ = await model.setItems("WippData", .all)
        _ = await first.value
        #expect(api.sets.count == 1)
    }

    @Test func reposLoadOncePerOwner() async {
        let api = FakeGitHub()
        let model = GitHubModel(api: api)
        #expect(model.reposFor("WippData") == nil)
        await model.loadRepos("WippData")
        await model.loadRepos("wippdata")
        #expect(api.repoLoads == ["WippData"] && model.reposFor("WIPPDATA")?.count == 2)
        api.failRepos = true
        await model.loadRepos("other")
        #expect(model.repoError("other") == "unavailable" && model.reposFor("other") == nil)
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
        try await Task.sleep(for: .milliseconds(150))
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
        api.setDelay = .milliseconds(200)
        let model = GitHubModel(api: api)
        await model.refresh()
        let save = Task { await model.setEnabled("WippData", true) }
        try? await Task.sleep(for: .milliseconds(50))
        // The server still has the old value; the reload must not show it.
        await model.refresh()
        #expect(model.saving.contains("WippData"))
        _ = await save.value
        #expect(model.scopes[1].enabled)
    }

    @Test func refreshDoesNotOverlap() async {
        let api = FakeGitHubForRobustness()
        api.scopesDelay = .milliseconds(150)
        let model = GitHubModel(api: api)
        let first = Task { await model.refresh() }
        try? await Task.sleep(for: .milliseconds(40))
        #expect(model.isRefreshing)
        await model.refresh()
        await first.value
        #expect(api.scopeLoads == 1 && !model.isRefreshing)
    }
}

private final class FakeGitHubForRobustness: GitHubAPI, @unchecked Sendable {
    var failItems = false, failRepos = false, failSet = false
    var setDelay: Duration = .zero, scopesDelay: Duration = .zero
    private(set) var scopeLoads = 0

    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity { GitHubIdentity(login: "willesq") }

    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList {
        scopeLoads += 1
        try? await Task.sleep(for: scopesDelay)
        return GitHubScopeList(scopes: [
            GitHubScope(owner: "willesq", isUser: true, enabled: true),
            GitHubScope(owner: "WippData", isUser: false),
        ])
    }

    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope {
        try? await Task.sleep(for: setDelay)
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
}

private struct RobustnessBoom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }
