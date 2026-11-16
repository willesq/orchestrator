import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

private struct Boom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

/// Pages `count` chunks out with a numeric cursor, like orchd; the test
/// controls failures and how long a page takes.
private final class PagedMemory: MemoryAPI, @unchecked Sendable {
    private let lock = NSLock()
    var count = 5
    var failBrowse = false
    var failCollections = false
    /// Delay per cursor ("" is the first page).
    var delays: [String: Duration] = [:]
    var collections = CollectionList(collections: [CollectionInfo(name: "base", chunks: 5)], spacesOn: false)
    private(set) var browses: [(BrowseQuery, String)] = []
    private(set) var collectionCalls = 0

    func search(_ query: SearchQuery) async throws -> SearchResult { SearchResult(hits: []) }

    func listSources(_ query: SourcesQuery) async throws -> SourceList {
        SourceList(sources: [], totalSources: 0, totalChunks: 0)
    }

    func listCollections() async throws -> CollectionList {
        lock.withLock { collectionCalls += 1 }
        if failCollections { throw Boom() }
        return collections
    }

    func browseChunks(_ query: BrowseQuery, cursor: String) async throws -> BrowsePage {
        lock.withLock { browses.append((query, cursor)) }
        if let delay = delays[cursor] { try? await Task.sleep(for: delay) }
        if failBrowse { throw Boom() }
        let start = Int(cursor) ?? 0
        let end = min(start + query.pageSize, count)
        let hits = (start..<end).map { SearchHit(text: "\($0)", sourceURI: query.sourceURI.isEmpty ? "u\($0)" : query.sourceURI) }
        return BrowsePage(hits: hits, nextCursor: end < count ? "\(end)" : "", total: cursor.isEmpty ? count : nil)
    }
}

@MainActor
@Suite struct BrowseModelTests {
    @Test func pagesToTheEndAndStops() async {
        let api = PagedMemory()
        let model = BrowseModel(api: api)
        model.query.pageSize = 2
        await model.load()
        #expect(model.hits.map(\.text) == ["0", "1"] && model.total == 5 && model.hasMore)
        #expect(model.footer == "Showing 2 of 5")
        await model.rowAppeared(at: 0)
        #expect(model.hits.count == 2, "only the last row loads more")
        await model.rowAppeared(at: 1)
        await model.loadMore()
        #expect(model.hits.map(\.text) == ["0", "1", "2", "3", "4"])
        #expect(!model.hasMore && model.total == 5, "a later page's zero total does not replace the count")
        #expect(model.footer == "All 5 shown")
        await model.loadMore()
        #expect(api.browses.map(\.1) == ["", "2", "4"], "nothing is asked after the last page")
    }

    @Test func typedFiltersApplyAndWordsAreNoted() async throws {
        let api = PagedMemory()
        let model = BrowseModel(api: api)
        model.query.sourceType = "web"
        model.filterText = "type:repo_doc repo:revok stray words"
        await model.load()
        let sent = try #require(api.browses.last?.0)
        #expect(sent.sourceType == "repo_doc" && sent.allTags == ["repo:revok"] && sent.tags.isEmpty && sent.pageSize == 50)
        #expect(model.parsed.notes.count == 1 && model.parsed.notes[0].contains("stray words"))
        await model.remove(model.parsed.chips[0])
        #expect(model.filterText == "repo:revok stray words" && api.browses.last?.0.sourceType == "web")
    }

    @Test func withinSourceSetsTheURIFilter() async {
        let api = PagedMemory()
        let model = BrowseModel(api: api)
        model.filterText = "uri:old repo:r"
        await model.within(source: "repo://o/r/a.md")
        #expect(model.filterText == "repo:r uri:repo://o/r/a.md")
        #expect(api.browses.last?.0.sourceURI == "repo://o/r/a.md")
        await model.within(source: "odd\"uri")
        #expect(model.filterText == "repo:r" && model.query.sourceURI == "odd\"uri")
        #expect(api.browses.last?.0.sourceURI == "odd\"uri")
    }

    @Test func aStalePageCannotLand() async {
        let api = PagedMemory()
        api.delays["2"] = .milliseconds(300)
        let model = BrowseModel(api: api)
        model.query.pageSize = 2
        await model.load()
        let slow = Task { await model.loadMore() }
        try? await Task.sleep(for: .milliseconds(50))
        #expect(model.isLoading)
        model.query.pageSize = 3
        await model.load()
        await slow.value
        #expect(model.hits.map(\.text) == ["0", "1", "2"], "the old listing's second page was dropped")
        #expect(!model.isLoading)
    }

    @Test func loadMoreWhileLoadingIsIgnored() async {
        let api = PagedMemory()
        api.delays["2"] = .milliseconds(200)
        let model = BrowseModel(api: api)
        model.query.pageSize = 2
        await model.load()
        let first = Task { await model.loadMore() }
        try? await Task.sleep(for: .milliseconds(50))
        await model.loadMore()
        await first.value
        #expect(api.browses.map(\.1) == ["", "2"])
    }

    @Test func errorsAreReportedAndRetryable() async {
        let api = PagedMemory()
        let model = BrowseModel(api: api)
        model.query.pageSize = 2
        await model.load()
        api.failBrowse = true
        await model.loadMore()
        #expect(model.lastError == "unavailable" && model.hits.count == 2 && model.hasMore && !model.isLoading)
        api.failBrowse = false
        await model.loadMore()
        #expect(model.lastError == nil && model.hits.count == 4)
    }

    @Test func emptyListingHasNoFooter() async {
        let api = PagedMemory()
        api.count = 0
        let model = BrowseModel(api: api)
        #expect(model.footer == nil && !model.hasMore)
        await model.load()
        #expect(model.loaded && model.hits.isEmpty && model.total == 0 && model.footer == nil)
    }
}

@MainActor
@Suite struct CollectionsModelTests {
    @Test func loadsOnceAndRefreshes() async {
        let api = PagedMemory()
        api.collections = CollectionList(collections: [CollectionInfo(name: "base", chunks: 3),
                                                       CollectionInfo(name: "orch-work", space: "work", chunks: 2)],
                                         spacesOn: true)
        let model = CollectionsModel(api: api)
        await model.loadOnce()
        await model.loadOnce()
        #expect(api.collectionCalls == 1 && model.spacesOn && model.collections.count == 2)
        #expect(model.label(for: "orch-work") == "work: orch-work — 2 chunks")
        #expect(model.label(for: "gone") == "gone")
        await model.refresh()
        #expect(api.collectionCalls == 2)
    }

    @Test func failureIsReportedAndKeepsTheList() async {
        let api = PagedMemory()
        let model = CollectionsModel(api: api)
        api.failCollections = true
        await model.loadOnce()
        #expect(!model.loaded && model.lastError == "unavailable")
        api.failCollections = false
        await model.loadOnce()
        #expect(model.loaded && model.lastError == nil && model.collections.map(\.name) == ["base"])
    }
}

/// GetDocument with a controllable delay and failure.
private final class Documents: CatalogAPI, @unchecked Sendable {
    var delays: [String: Duration] = [:]
    var fail = false

    func document(uri: String) async throws -> RebuiltDocument {
        if let delay = delays[uri] { try? await Task.sleep(for: delay) }
        if fail { throw Boom() }
        return RebuiltDocument(sourceURI: uri, text: "text of \(uri)", chunks: 2)
    }

    func listDocs() async throws -> DocCatalog { throw Boom() }
    func syncDocs(owner: String?, force: Bool) async throws -> DocsSyncState { throw Boom() }
    func docsSync() async throws -> DocsSyncState { throw Boom() }
    func registeredSources() async throws -> [RegisteredSource] { [] }
    func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource { throw Boom() }
    func removeSource(id: String, purge: Bool) async throws -> PurgeResult { throw Boom() }
    func refreshSource(id: String?) async throws {}
}

@MainActor
@Suite struct SourceDocumentModelTests {
    @Test func opensAndCloses() async {
        let model = SourceDocumentModel(api: Documents())
        await model.open("repo://a")
        #expect(model.uri == "repo://a" && model.document?.text == "text of repo://a" && !model.isLoading)
        model.close()
        #expect(model.uri == nil && model.document == nil)
    }

    @Test func aSlowerEarlierOpenCannotReplaceALaterOne() async {
        let api = Documents()
        api.delays["slow"] = .milliseconds(300)
        let model = SourceDocumentModel(api: api)
        let slow = Task { await model.open("slow") }
        try? await Task.sleep(for: .milliseconds(50))
        await model.open("fast")
        await slow.value
        #expect(model.document?.sourceURI == "fast" && model.uri == "fast")
    }

    @Test func failureIsReported() async {
        let api = Documents()
        api.fail = true
        let model = SourceDocumentModel(api: api)
        await model.open("x")
        #expect(model.lastError == "unavailable" && model.document == nil && !model.isLoading)
        api.fail = false
        api.delays["late"] = .milliseconds(200)
        api.fail = true
        let late = Task { await model.open("late") }
        try? await Task.sleep(for: .milliseconds(50))
        model.close()
        await late.value
        #expect(model.lastError == nil && model.uri == nil, "a failure after closing is dropped")
    }
}

@Suite struct MemoryClientTests {
    private func withClient(_ services: [any RegistrableRPCService], _ body: (DaemonClient) async throws -> Void) async throws {
        try await withFakeOrchd(services: services) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            try await body(client)
        }
    }

    @Test func searchSendsEveryFilterAndMapsEveryField() async throws {
        var hit = Orchd_V1_SearchHit()
        hit.text = "chunk"
        hit.sourceUri = "repo://o/r/README.md"
        hit.heading = "Install"
        hit.trust = "repo"
        hit.space = "work"
        hit.tenant = "slack:T1"
        hit.collection = "orch-work"
        hit.pointID = "p-1"
        hit.chunkIndex = 4
        hit.startedAt = 10
        hit.endedAt = 20
        hit.ingestedAt = 30
        var bare = Orchd_V1_SearchHit()
        bare.sourceUri = "u"
        var service = FakeOrchdService()
        service.search.hits = [hit, bare]
        service.search.collections = ["orch-work"]
        service.search.scoped = true
        try await withClient([service]) { client in
            let result = try await client.search(SearchQuery(
                text: "q", tags: ["repo:r", ""], collections: ["orch-work"],
                since: Date(timeIntervalSince1970: 100), until: Date(timeIntervalSince1970: 200)))
            #expect(result.collections == ["orch-work"] && result.scoped)
            #expect(result.hits[0] == SearchHit(
                text: "chunk", sourceURI: "repo://o/r/README.md", startedAt: Date(timeIntervalSince1970: 10),
                endedAt: Date(timeIntervalSince1970: 20), ingestedAt: Date(timeIntervalSince1970: 30), heading: "Install",
                trust: "repo", space: "work", tenant: "slack:T1", collection: "orch-work", pointID: "p-1", chunkIndex: 4))
            #expect(result.hits[1].chunkIndex == nil, "an absent chunk index stays unknown, not 0")
            let sent = try #require(service.recorder.last(Orchd_V1_SearchRequest.self))
            #expect(sent.tags == ["repo:r"] && sent.collections == ["orch-work"] && sent.since == 100 && sent.until == 200)
        }
    }

    @Test func listSourcesSendsCollections() async throws {
        let service = FakeOrchdService()
        try await withClient([service]) { client in
            _ = try await client.listSources(type: "web", limit: 5, collections: ["a", ""])
            let sent = try #require(service.recorder.last(Orchd_V1_ListSourcesRequest.self))
            #expect(sent.collections == ["a"] && sent.sourceType == "web")
            _ = try await client.listSources(type: "", limit: 5)
            #expect(service.recorder.last(Orchd_V1_ListSourcesRequest.self)?.collections == [])
            _ = try await client.listSources(SourcesQuery(
                sourceType: " web ", scope: "o/r", uriPrefix: "session://", collections: ["c", ""], tags: ["any"],
                allTags: ["repo:r", ""], since: Date(timeIntervalSince1970: 100), until: Date(timeIntervalSince1970: 200),
                limit: 0))
            let full = try #require(service.recorder.last(Orchd_V1_ListSourcesRequest.self))
            #expect(full.sourceType == "web" && full.scope == "o/r" && full.uriPrefix == "session://")
            #expect(full.collections == ["c"] && full.tags == ["any"] && full.allTags == ["repo:r"])
            #expect(full.since == 100 && full.until == 200 && full.limit == 0, "0 lists every source")
        }
        #expect(DaemonClient.listTimeout == .seconds(30))
    }

    @Test func listCollectionsMapsSpaces() async throws {
        let service = FakeMemoryBrowseService(count: 0)
        try await withClient([service]) { client in
            let list = try await client.listCollections()
            #expect(list == CollectionList(collections: [CollectionInfo(name: "base", chunks: 7, hybrid: true),
                                                         CollectionInfo(name: "orch-work", space: "work", chunks: 2)],
                                           spacesOn: true))
        }
    }

    @Test func browseSendsFiltersAndPagesToTheEnd() async throws {
        let service = FakeMemoryBrowseService(count: 5)
        try await withClient([service]) { client in
            let model = await BrowseModel(api: client)
            await MainActor.run {
                model.query = BrowseQuery(collections: ["orch-work"], allTags: ["owner:o"], pageSize: 2)
                model.filterText = "type:repo_doc scope:o/r uri:repo://o/r tag:x since:2026-11-01 until:2026-11-02"
            }
            await model.load()
            while await model.hasMore { await model.loadMore() }
            let (hits, total) = await MainActor.run { (model.hits, model.total) }
            #expect(hits.map(\.pointID) == ["p0", "p1", "p2", "p3", "p4"] && total == 5)
            #expect(hits[0].chunkIndex == 0 && hits[0].collection == "orch-work")
            let sent = try #require(service.recorder.last(Orchd_V1_BrowseChunksRequest.self))
            #expect(sent.cursor == "4" && sent.limit == 2 && sent.collections == ["orch-work"])
            #expect(sent.sourceType == "repo_doc" && sent.scope == "o/r" && sent.sourceUri == "repo://o/r")
            #expect(sent.tags.isEmpty && sent.allTags == ["owner:o", "x"] && sent.since > 0 && sent.until > sent.since)
        }
    }
}

/// ListCollections and BrowseChunks served like orchd, paging `count`
/// chunks with a numeric cursor.
struct FakeMemoryBrowseService: RegistrableRPCService {
    let count: Int
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let count = self.count, recorder = self.recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListCollections.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListCollectionsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListCollectionsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            var base = Orchd_V1_CollectionInfo()
            base.name = "base"
            base.chunks = 7
            base.hybrid = true
            var work = Orchd_V1_CollectionInfo()
            work.name = "orch-work"
            work.space = "work"
            work.chunks = 2
            var response = Orchd_V1_ListCollectionsResponse()
            response.collections = [base, work]
            response.spacesOn = true
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.BrowseChunks.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_BrowseChunksRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_BrowseChunksResponse>()
        ) { request, _ in
            var received = Orchd_V1_BrowseChunksRequest()
            for try await message in request.messages {
                recorder.record(message)
                received = message
            }
            let start = Int(received.cursor) ?? 0
            let end = min(start + Int(received.limit == 0 ? 50 : received.limit), count)
            var response = Orchd_V1_BrowseChunksResponse()
            response.hits = (start..<end).map { i in
                var hit = Orchd_V1_SearchHit()
                hit.sourceUri = received.sourceUri
                hit.collection = received.collections.first ?? "base"
                hit.pointID = "p\(i)"
                hit.chunkIndex = Int32(i)
                return hit
            }
            response.nextCursor = end < count ? "\(end)" : ""
            response.total = received.cursor.isEmpty ? Int64(count) : 0
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
    }
}
