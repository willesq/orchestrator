import Foundation

/// One vector-store collection the app may read: the base collection, and
/// with spaces on each space's own.
public struct CollectionInfo: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    /// The space the collection holds; empty for the base collection.
    public let space: String
    public let chunks: Int
    /// The collection has a keyword index, so hybrid search can run on it.
    public let hybrid: Bool

    public init(name: String, space: String = "", chunks: Int = 0, hybrid: Bool = false) {
        self.name = name
        self.space = space
        self.chunks = chunks
        self.hybrid = hybrid
    }

    /// The menu label: "orchestrator — 1,234 chunks", with the space first
    /// when the collection holds one.
    public func label(spacesOn: Bool) -> String {
        let count = "\(chunks.formatted()) chunk\(chunks == 1 ? "" : "s")"
        let name = displaySafe(self.name)
        if spacesOn && !space.isEmpty { return "\(displaySafe(space)): \(name) — \(count)" }
        return "\(name) — \(count)"
    }
}

public struct CollectionList: Sendable, Equatable {
    /// The base collection first, then by name, as orchd lists them.
    public let collections: [CollectionInfo]
    /// Spaces are on, so each space has its own collection.
    public let spacesOn: Bool

    public init(collections: [CollectionInfo], spacesOn: Bool) {
        self.collections = collections
        self.spacesOn = spacesOn
    }
}

/// What Browse lists: every stored chunk matching these exact-match
/// filters, in storage order. There is no text to rank by.
public struct BrowseQuery: Sendable, Equatable {
    /// orchd's largest page.
    public static let maxPageSize = 200
    /// The page sizes the Browse view offers.
    public static let pageSizeChoices = [25, 50, 100, 200]

    public var collections: [String]
    public var sourceType: String
    public var scope: String
    public var sourceURI: String
    /// Any of these tags, and every one of `allTags`.
    public var tags: [String]
    public var allTags: [String]
    public var since: Date?
    public var until: Date?
    public var pageSize: Int

    public init(collections: [String] = [], sourceType: String = "", scope: String = "", sourceURI: String = "",
                tags: [String] = [], allTags: [String] = [], since: Date? = nil, until: Date? = nil, pageSize: Int = 50) {
        self.collections = collections
        self.sourceType = sourceType
        self.scope = scope
        self.sourceURI = sourceURI
        self.tags = tags
        self.allTags = allTags
        self.since = since
        self.until = until
        self.pageSize = pageSize
    }

    /// The query as sent: trimmed filters, empty entries dropped, a page
    /// size in 1...maxPageSize.
    public var normalized: BrowseQuery {
        BrowseQuery(
            collections: collections.filter { !$0.isEmpty },
            sourceType: sourceType.trimmingCharacters(in: .whitespaces),
            scope: scope.trimmingCharacters(in: .whitespaces),
            sourceURI: sourceURI.trimmingCharacters(in: .whitespaces),
            tags: tags.filter { !$0.isEmpty },
            allTags: allTags.filter { !$0.isEmpty },
            since: since, until: until,
            pageSize: min(max(pageSize, 1), Self.maxPageSize)
        )
    }
}

/// One page of a browse.
public struct BrowsePage: Sendable, Equatable {
    /// Unscored: score, keyword score and exact are zero.
    public let hits: [SearchHit]
    /// Continues the listing; empty when this page is the last.
    public let nextCursor: String
    /// How many chunks match, counted on the first page only.
    public let total: Int?

    public init(hits: [SearchHit], nextCursor: String = "", total: Int? = nil) {
        self.hits = hits
        self.nextCursor = nextCursor
        self.total = total
    }
}

/// Unix seconds for orchd, where 0 means an open bound.
func unixSeconds(_ date: Date?) -> Int64 {
    guard let date else { return 0 }
    return max(Int64(date.timeIntervalSince1970.rounded(.down)), 0)
}
