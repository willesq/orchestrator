import Foundation
import Observation

/// What the Indexed tab lists: the stored sources matching these filters,
/// with their chunk counts. orchd's ListSources takes every filter Search
/// and Browse do, except that a source address is a prefix.
public struct SourcesQuery: Sendable, Equatable {
    /// The limits the Indexed tab offers; 0 lists every matching source.
    public static let limitChoices = [50, 200, 1000, 0]
    public static let defaultLimit = 200

    public var sourceType: String
    public var scope: String
    /// Only sources whose address starts with this, e.g. `session://`.
    public var uriPrefix: String
    public var collections: [String]
    /// Any of these tags, and every one of `allTags`.
    public var tags: [String]
    public var allTags: [String]
    public var since: Date?
    public var until: Date?
    /// The most sources listed; 0 for no cap.
    public var limit: Int

    public init(sourceType: String = "", scope: String = "", uriPrefix: String = "", collections: [String] = [],
                tags: [String] = [], allTags: [String] = [], since: Date? = nil, until: Date? = nil,
                limit: Int = defaultLimit) {
        self.sourceType = sourceType
        self.scope = scope
        self.uriPrefix = uriPrefix
        self.collections = collections
        self.tags = tags
        self.allTags = allTags
        self.since = since
        self.until = until
        self.limit = limit
    }

    /// The query as sent: trimmed filters, empty entries dropped, a limit
    /// of at least 0.
    public var normalized: SourcesQuery {
        SourcesQuery(
            sourceType: sourceType.trimmingCharacters(in: .whitespaces),
            scope: scope.trimmingCharacters(in: .whitespaces),
            uriPrefix: uriPrefix.trimmingCharacters(in: .whitespaces),
            collections: collections.filter { !$0.isEmpty },
            tags: tags.filter { !$0.isEmpty },
            allTags: allTags.filter { !$0.isEmpty },
            since: since, until: until,
            limit: max(limit, 0)
        )
    }

    /// "200 at most", or "All" for no cap.
    public static func label(forLimit limit: Int) -> String { limit == 0 ? "All" : "\(limit) at most" }
}

extension ParsedMemoryQuery {
    /// The same for a sources listing: a typed `uri:` is a prefix, and
    /// `tag:` and `repo:` must all match, as in Search and Browse.
    public func applied(to query: SourcesQuery) -> SourcesQuery {
        var q = query
        if !sourceType.isEmpty { q.sourceType = sourceType }
        if !scope.isEmpty { q.scope = scope }
        if !sourceURI.isEmpty { q.uriPrefix = sourceURI }
        if !collections.isEmpty { q.collections = collections }
        q.allTags += tags
        q.since = since ?? q.since
        q.until = until ?? q.until
        return q
    }
}

/// Drives the Memory pane's Indexed tab: the stored sources the filters
/// match, up to a limit or all of them.
@MainActor
@Observable
public final class SourcesModel {
    /// The default limit, kept for callers that name it.
    public static let pageSize = SourcesQuery.defaultLimit

    /// The Type menu's choice; a typed `type:` replaces it.
    public var typeFilter = ""
    /// Only these collections; empty lists every readable one.
    public var collections: [String] = []
    /// `key:value` filters typed in the filter field; words are ignored.
    public var filterText = ""
    /// The most sources listed; 0 lists them all.
    public var limit = SourcesQuery.defaultLimit
    public private(set) var parsed = ParsedMemoryQuery()
    public private(set) var list: SourceList?
    public private(set) var lastError: String?
    /// The query `list` answers.
    public private(set) var answered: SourcesQuery?

    private let api: any MemoryAPI
    /// Bumped per listing, so an older one answering late is dropped.
    private var generation = 0

    public init(api: any MemoryAPI) {
        self.api = api
    }

    /// Lists again with the menus, the typed filters and the limit.
    public func refresh() async {
        generation += 1
        let mine = generation
        parsed = MemoryFilterParser.parse(filterText)
        if !parsed.text.isEmpty {
            parsed.notes.append("Indexed lists by filters only, so \u{201C}\(displaySafe(parsed.text))\u{201D} was ignored; "
                + "use Search to rank by words.")
        }
        let q = parsed.applied(to: SourcesQuery(sourceType: typeFilter, collections: collections, limit: limit)).normalized
        do {
            let fresh = try await api.listSources(q)
            guard mine == generation else { return }
            list = fresh
            answered = q
            seenTypes.formUnion(fresh.sources.map(\.sourceType).filter { !$0.isEmpty })
            lastError = nil
        } catch {
            guard mine == generation else { return }
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Drops one typed filter and lists again.
    public func remove(_ chip: MemoryFilterChip) async {
        filterText = MemoryFilterParser.removing(chip, from: filterText)
        await refresh()
    }

    /// "200 of 1,234 sources · 9,876 chunks", or "All 12 sources · 40
    /// chunks" when nothing was cut off.
    public var summary: String? {
        guard let list else { return nil }
        if list.totalSources == 0 { return "No sources match" }
        let chunks = "\(list.totalChunks.formatted()) chunk\(list.totalChunks == 1 ? "" : "s")"
        let noun = list.totalSources == 1 ? "source" : "sources"
        if list.sources.count < list.totalSources {
            return "\(list.sources.count.formatted()) of \(list.totalSources.formatted()) \(noun) · \(chunks)"
        }
        return "All \(list.totalSources.formatted()) \(noun) · \(chunks)"
    }

    /// Every source type seen in any listing, so filtering to one type keeps
    /// the others in the menu.
    private var seenTypes: Set<String> = []

    /// Source types seen so far, for the filter menus.
    public var knownTypes: [String] { seenTypes.sorted() }
}
