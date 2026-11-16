import Foundation
import Testing
@testable import OrchKit

@Suite struct SearchQueryTests {
    @Test(arguments: [(0, 1), (-5, 1), (10, 10), (100, 100), (101, 100), (Int.max, 100)])
    func limitIsClamped(raw: Int, sent: Int) {
        #expect(SearchQuery(text: "x", limit: raw).normalized.limit == sent)
    }

    @Test func textAndFiltersAreTrimmed() {
        let q = SearchQuery(text: "  hybrid search \n", sourceType: " repo_doc ", scope: " o/r ", mode: .dense).normalized
        #expect(q == SearchQuery(text: "hybrid search", limit: 10, sourceType: "repo_doc", scope: "o/r", mode: .dense))
    }

    @Test func modesMapToTheWireValues() {
        #expect(SearchMode.allCases.map(\.rawValue) == ["", "hybrid", "dense"])
        #expect(Set(SearchMode.allCases.map(\.label)).count == 3)
    }
}

private struct Boom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

/// A memory API whose answers the test controls, including how long they take.
private final class FakeMemory: MemoryAPI, @unchecked Sendable {
    private let lock = NSLock()
    var delays: [String: Duration] = [:]
    var failSearch = false
    var failSources = false
    private(set) var queries: [SearchQuery] = []
    private(set) var sourceRequests: [(String, Int, [String])] = []
    /// The hits each search answers with; one per query by default.
    var hits: [SearchHit]?

    func search(_ query: SearchQuery) async throws -> SearchResult {
        lock.withLock { queries.append(query) }
        if let delay = delays[query.text] { try? await Task.sleep(for: delay) }
        if failSearch { throw Boom() }
        return SearchResult(hits: hits ?? [SearchHit(text: "about \(query.text)", sourceURI: "repo://\(query.text)")])
    }

    func listSources(type: String, limit: Int, collections: [String]) async throws -> SourceList {
        lock.withLock { sourceRequests.append((type, limit, collections)) }
        if failSources { throw Boom() }
        return SourceList(sources: [
            SourceSummary(uri: "repo://a", sourceType: "repo_doc"),
            SourceSummary(uri: "session://b", sourceType: "session_transcript"),
            SourceSummary(uri: "manual://c", sourceType: ""),
            SourceSummary(uri: "repo://d", sourceType: "repo_doc"),
        ], totalSources: 4, totalChunks: 9)
    }

    func listCollections() async throws -> CollectionList { CollectionList(collections: [], spacesOn: false) }
    func browseChunks(_ query: BrowseQuery, cursor: String) async throws -> BrowsePage { BrowsePage(hits: []) }
}

@MainActor
@Suite struct SearchModelTests {
    @Test func searchPublishesResult() async {
        let api = FakeMemory()
        let model = SearchModel(api: api)
        model.query = SearchQuery(text: "  qdrant ", limit: 500)
        await model.search()
        #expect(model.result?.hits.first?.sourceURI == "repo://qdrant")
        #expect(model.answered?.text == "qdrant")
        #expect(api.queries.first?.limit == 100)
        #expect(!model.isSearching && model.lastError == nil)
    }

    @Test func emptyQueryClearsWithoutCallingOrchd() async {
        let api = FakeMemory()
        let model = SearchModel(api: api)
        model.query.text = "x"
        await model.search()
        model.query.text = "   "
        await model.search()
        #expect(model.result == nil && model.answered == nil)
        #expect(api.queries.count == 1)
    }

    @Test func failureReportsAndKeepsPreviousResult() async {
        let api = FakeMemory()
        let model = SearchModel(api: api)
        model.query.text = "first"
        await model.search()
        api.failSearch = true
        model.query.text = "second"
        await model.search()
        #expect(model.lastError == "unavailable")
        #expect(model.answered?.text == "first")
        #expect(!model.isSearching)
    }

    @Test func aStaleSearchCannotOverwriteANewerOne() async {
        let api = FakeMemory()
        api.delays["slow"] = .milliseconds(300)
        let model = SearchModel(api: api)
        model.query.text = "slow"
        let slow = Task { await model.search() }
        try? await Task.sleep(for: .milliseconds(30))
        model.query.text = "fast"
        await model.search()
        #expect(model.answered?.text == "fast")
        await slow.value
        #expect(model.answered?.text == "fast", "the slow search finished last but must not win")
        #expect(!model.isSearching)
    }

    @Test func pagedFetchesOneBatchAndRevealsItWithoutAskingAgain() async {
        let api = FakeMemory()
        api.hits = (0..<100).map { SearchHit(text: "\($0)", sourceURI: "u\($0)", score: Float(100 - $0) / 100) }
        let model = SearchModel(api: api, paged: true)
        model.query = SearchQuery(text: "retry", limit: 20)
        await model.search()
        #expect(api.queries.count == 1 && api.queries[0].limit == SearchQuery.maxResults)
        #expect(model.visibleHits.map(\.sourceURI) == (0..<20).map { "u\($0)" })
        #expect(model.footer == "Showing 20 of 100 — ranked search returns at most 100; use Browse to see every match")
        model.showMore()
        model.showMore()
        #expect(model.visibleHits.count == 60 && model.visibleHits[20].sourceURI == "u20")
        model.query.limit = 50
        #expect(model.visibleHits.count == 100 && !model.canShowMore)
        model.showMore()
        #expect(model.visibleHits.count == 100)
        #expect(api.queries.count == 1, "revealing pages never asks orchd again")
        await model.search()
        #expect(model.visibleHits.count == 50, "a new search starts at the first page")
    }

    @Test func minimumRelevanceFiltersTheBatch() async {
        let api = FakeMemory()
        api.hits = [0.9, 0.5, 0.3, 0.1].map { SearchHit(text: "", sourceURI: "s\($0)", score: Float($0)) }
        let model = SearchModel(api: api, paged: true)
        model.query.text = "x"
        await model.search()
        #expect(model.footer == "Showing 4 of 4")
        model.minRelevance = 0.4
        #expect(model.visibleHits.map(\.sourceURI) == ["s0.9", "s0.5"])
        #expect(model.hiddenByRelevance == 2)
        #expect(model.footer == "Showing 2 of 2 · 2 below relevance 0.4 hidden")
        #expect(api.queries.count == 1)
    }

    @Test func unpagedShowsEveryHitAndSendsTheLimit() async {
        let api = FakeMemory()
        api.hits = (0..<30).map { SearchHit(text: "", sourceURI: "u\($0)") }
        let model = SearchModel(api: api)
        model.query = SearchQuery(text: "type:repo_doc x", limit: 10)
        await model.search()
        #expect(api.queries[0].limit == 10 && api.queries[0].text == "type:repo_doc x", "only the Memory pane reads filters")
        #expect(model.visibleHits.count == 30 && model.footer == nil)
    }

    @Test func typedFiltersReachTheQueryAndChipsRemoveThem() async throws {
        let api = FakeMemory()
        let model = SearchModel(api: api, paged: true)
        model.query = SearchQuery(text: "retry repo:revok type:repo_doc collection:base", sourceType: "web")
        await model.search()
        let sent = try #require(api.queries.last)
        #expect(sent.text == "retry" && sent.sourceType == "repo_doc" && sent.allTags == ["repo:revok"] && sent.tags.isEmpty)
        #expect(sent.collections == ["base"])
        #expect(model.parsed.chips.map(\.key) == [.repo, .type, .collection])
        #expect(model.recent == ["retry repo:revok type:repo_doc collection:base"])
        await model.remove(model.parsed.chips[1])
        #expect(model.query.text == "retry repo:revok collection:base")
        #expect(api.queries.last?.sourceType == "web", "the menu's type applies again")
    }

    @Test func filtersWithoutWordsAskForWords() async {
        let api = FakeMemory()
        let model = SearchModel(api: api, paged: true)
        model.query.text = "type:repo_doc"
        await model.search()
        #expect(api.queries.isEmpty && model.result == nil)
        #expect(model.parsed.notes.last?.hasPrefix("Add words") == true)
    }

    @Test func withinSourceReplacesTheURIFilter() async {
        let api = FakeMemory()
        let model = SearchModel(api: api, paged: true)
        model.query.text = "retry uri:a://old"
        await model.within(source: "file:///tmp/a b.md")
        #expect(model.query.text == "retry uri:\"file:///tmp/a b.md\"")
        #expect(api.queries.last?.sourceURI == "file:///tmp/a b.md")
        model.query.text = ""
        await model.within(source: "repo://o/r")
        #expect(model.query.text == "uri:repo://o/r" && api.queries.count == 1, "no words: set, not run")
        await model.within(source: "bad\"uri")
        #expect(model.query.text == "uri:repo://o/r", "an address no token can carry is not set")
    }

    @Test func isSearchingWhileInFlight() async {
        let api = FakeMemory()
        api.delays["slow"] = .milliseconds(200)
        let model = SearchModel(api: api)
        model.query.text = "slow"
        let task = Task { await model.search() }
        try? await Task.sleep(for: .milliseconds(50))
        #expect(model.isSearching)
        await task.value
        #expect(!model.isSearching)
    }
}

@MainActor
@Suite struct SourcesModelTests {
    @Test func refreshListsAndPassesTrimmedFilter() async {
        let api = FakeMemory()
        let model = SourcesModel(api: api)
        model.typeFilter = " repo_doc "
        await model.refresh()
        #expect(model.list?.totalSources == 4)
        #expect(api.sourceRequests.first?.0 == "repo_doc")
        #expect(api.sourceRequests.first?.1 == SourcesModel.pageSize)
        #expect(api.sourceRequests.first?.2 == [])
        #expect(model.knownTypes == ["repo_doc", "session_transcript"])
    }

    @Test func failureKeepsLastList() async {
        let api = FakeMemory()
        let model = SourcesModel(api: api)
        #expect(model.knownTypes.isEmpty)
        await model.refresh()
        api.failSources = true
        await model.refresh()
        #expect(model.list?.sources.count == 4 && model.lastError == "unavailable")
    }
}

private final class FakeControl: ControlAPI, @unchecked Sendable {
    var fail = false
    var delay: Duration = .zero
    private(set) var calls: [String?] = []

    func restart(_ name: String?) async throws -> [String] {
        calls.append(name)
        try? await Task.sleep(for: delay)
        if fail { throw Boom() }
        return name.map { [$0] } ?? ["qdrant", "handoff"]
    }
}

@MainActor
@Suite struct RestartModelTests {
    @Test func restartOneThenAll() async {
        let api = FakeControl()
        var refreshed = 0
        let model = RestartModel(api: api) { refreshed += 1 }
        await model.restart("qdrant")
        #expect(model.lastMessage == "Restarted qdrant" && !model.lastFailed)
        await model.restart(nil)
        #expect(model.lastMessage == "Restarted qdrant, handoff")
        #expect(api.calls == ["qdrant", nil] && refreshed == 2 && model.restarting == nil)
    }

    @Test func failureIsReportedAndStillRefreshes() async {
        let api = FakeControl()
        api.fail = true
        var refreshed = 0
        let model = RestartModel(api: api) { refreshed += 1 }
        await model.restart("qdrant")
        #expect(model.lastFailed && model.lastMessage == "Restart failed: unavailable" && refreshed == 1)
    }

    @Test func secondRestartWhileBusyIsIgnored() async {
        let api = FakeControl()
        api.delay = .milliseconds(200)
        let model = RestartModel(api: api)
        let first = Task { await model.restart("qdrant") }
        try? await Task.sleep(for: .milliseconds(50))
        #expect(model.restarting == "qdrant")
        await model.restart("handoff")
        await first.value
        #expect(api.calls == ["qdrant"])
    }

    @Test func emptyResultSaysSo() async {
        final class Empty: ControlAPI { func restart(_: String?) async throws -> [String] { [] } }
        let model = RestartModel(api: Empty())
        await model.restart(nil)
        #expect(model.lastMessage == "Nothing restarted")
    }
}
