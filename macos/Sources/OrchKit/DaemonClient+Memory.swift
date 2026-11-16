import Foundation
import GRPCCore

/// Searching, listing and browsing durable memory: the Memory pane's calls.
extension DaemonClient {
    /// Listing a large store walks every chunk: about 6 s for 60k chunks, so
    /// the 3 s default timed out.
    static let listTimeout: Duration = .seconds(30)

    public func search(_ query: SearchQuery) async throws -> SearchResult {
        var options = CallOptions.defaults
        options.timeout = searchTimeout
        let response = try await orchd.search(Self.searchRequest(query.normalized), options: options)
        return SearchResult(
            hits: response.hits.map(Self.hit),
            fallback: response.fallback.isEmpty ? nil : response.fallback,
            collections: response.collections,
            scoped: response.scoped
        )
    }

    public func listSources(type: String, limit: Int, collections: [String]) async throws -> SourceList {
        var request = Orchd_V1_ListSourcesRequest()
        request.sourceType = type
        request.limit = Int32(clamping: max(limit, 0))
        request.collections = collections.filter { !$0.isEmpty }
        let response = try await orchd.listSources(request, options: Self.longOptions)
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

    public func listCollections() async throws -> CollectionList {
        // Each collection's chunks are counted, which a large store answers slowly.
        let response = try await orchd.listCollections(Orchd_V1_ListCollectionsRequest(), options: Self.longOptions)
        return CollectionList(
            collections: response.collections.map {
                CollectionInfo(name: $0.name, space: $0.space, chunks: Int($0.chunks), hybrid: $0.hybrid)
            },
            spacesOn: response.spacesOn
        )
    }

    public func browseChunks(_ query: BrowseQuery, cursor: String) async throws -> BrowsePage {
        let q = query.normalized
        var request = Orchd_V1_BrowseChunksRequest()
        request.collections = q.collections
        request.sourceType = q.sourceType
        request.scope = q.scope
        request.sourceUri = q.sourceURI
        request.tags = q.tags
        request.allTags = q.allTags
        request.since = unixSeconds(q.since)
        request.until = unixSeconds(q.until)
        request.limit = Int32(q.pageSize)
        request.cursor = cursor
        // The first page also counts every match.
        let response = try await orchd.browseChunks(request, options: Self.longOptions)
        return BrowsePage(hits: response.hits.map(Self.hit), nextCursor: response.nextCursor,
                          total: cursor.isEmpty ? Int(response.total) : nil)
    }

    /// A normalized query as orchd's SearchRequest, shared with the
    /// direct-message search.
    static func searchRequest(_ q: SearchQuery) -> Orchd_V1_SearchRequest {
        var request = Orchd_V1_SearchRequest()
        request.query = q.text
        request.topK = Int32(q.limit)
        request.sourceType = q.sourceType
        request.scope = q.scope
        request.mode = q.mode.rawValue
        request.sourceUri = q.sourceURI
        request.allTags = q.allTags
        request.tags = q.tags
        request.collections = q.collections
        request.since = unixSeconds(q.since)
        request.until = unixSeconds(q.until)
        return request
    }

    /// Every field of a hit, as stored; the views make it display-safe.
    static func hit(_ h: Orchd_V1_SearchHit) -> SearchHit {
        SearchHit(
            text: h.text, sourceURI: h.sourceUri, sourceType: h.sourceType, scope: h.scope, score: h.score,
            keywordScore: h.keywordScore, exact: h.exact, tags: h.tags, startedAt: date(fromUnix: h.startedAt),
            endedAt: date(fromUnix: h.endedAt), ingestedAt: date(fromUnix: h.ingestedAt), heading: h.heading,
            trust: h.trust, space: h.space, tenant: h.tenant, collection: h.collection, pointID: h.pointID,
            chunkIndex: h.hasChunkIndex ? Int(h.chunkIndex) : nil
        )
    }

    private static var longOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = listTimeout
        return options
    }
}
