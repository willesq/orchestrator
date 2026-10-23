import Foundation

/// How orchd ranks a search: both semantic and keyword (hybrid), meaning only
/// (dense), or whatever `rag.search.mode` configures.
public enum SearchMode: String, Sendable, CaseIterable, Identifiable {
    case configured = ""
    case hybrid
    case dense

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .configured: "Default"
        case .hybrid: "Hybrid"
        case .dense: "Meaning only"
        }
    }
}

public struct SearchQuery: Sendable, Equatable {
    /// orchd rejects anything above this.
    public static let maxResults = 100

    public var text: String
    public var limit: Int
    public var sourceType: String
    public var scope: String
    public var mode: SearchMode

    public init(text: String, limit: Int = 10, sourceType: String = "", scope: String = "", mode: SearchMode = .configured) {
        self.text = text
        self.limit = limit
        self.sourceType = sourceType
        self.scope = scope
        self.mode = mode
    }

    /// The query as sent: trimmed text, trimmed filters, a limit in 1...maxResults.
    public var normalized: SearchQuery {
        SearchQuery(
            text: text.trimmingCharacters(in: .whitespacesAndNewlines),
            limit: min(max(limit, 1), Self.maxResults),
            sourceType: sourceType.trimmingCharacters(in: .whitespaces),
            scope: scope.trimmingCharacters(in: .whitespaces),
            mode: mode
        )
    }
}

/// One stored chunk that matched a search. Its text is indexed content (web
/// pages, repos, transcripts) and is shown as plain text only.
public struct SearchHit: Sendable, Equatable {
    public let text: String
    public let sourceURI: String
    public let sourceType: String
    public let scope: String
    public let score: Float
    public let keywordScore: Float
    public let exact: Bool
    public let tags: [String]
    /// When what the hit describes happened, if known.
    public let startedAt: Date?
    public let ingestedAt: Date?

    public init(
        text: String, sourceURI: String, sourceType: String = "", scope: String = "", score: Float = 0,
        keywordScore: Float = 0, exact: Bool = false, tags: [String] = [], startedAt: Date? = nil, ingestedAt: Date? = nil
    ) {
        self.text = text
        self.sourceURI = sourceURI
        self.sourceType = sourceType
        self.scope = scope
        self.score = score
        self.keywordScore = keywordScore
        self.exact = exact
        self.tags = tags
        self.startedAt = startedAt
        self.ingestedAt = ingestedAt
    }
}

public struct SearchResult: Sendable, Equatable {
    public let hits: [SearchHit]
    /// Why a hybrid search answered by meaning only; nil when it ran as asked.
    public let fallback: String?

    public init(hits: [SearchHit], fallback: String? = nil) {
        self.hits = hits
        self.fallback = fallback
    }
}

/// One stored source, as `orch rag sources` lists it.
public struct SourceSummary: Sendable, Equatable, Identifiable {
    public var id: String { uri }
    public let uri: String
    public let sourceType: String
    public let scope: String
    public let chunks: Int
    public let tags: [String]
    public let ingestedAt: Date?

    public init(uri: String, sourceType: String = "", scope: String = "", chunks: Int = 0, tags: [String] = [], ingestedAt: Date? = nil) {
        self.uri = uri
        self.sourceType = sourceType
        self.scope = scope
        self.chunks = chunks
        self.tags = tags
        self.ingestedAt = ingestedAt
    }
}

public struct SourceList: Sendable, Equatable {
    public let sources: [SourceSummary]
    /// Distinct sources and chunks matching the filter, before the limit.
    public let totalSources: Int
    public let totalChunks: Int

    public init(sources: [SourceSummary], totalSources: Int, totalChunks: Int) {
        self.sources = sources
        self.totalSources = totalSources
        self.totalChunks = totalChunks
    }
}

/// Searching and listing durable memory.
public protocol MemoryAPI: Sendable {
    func search(_ query: SearchQuery) async throws -> SearchResult
    func listSources(type: String, limit: Int) async throws -> SourceList
}

/// Restarting supervised orchd units in place.
public protocol ControlAPI: Sendable {
    /// Restarts one unit, or every unit when `name` is nil; returns the names restarted.
    func restart(_ name: String?) async throws -> [String]
}

/// Unix seconds from orchd, where 0 means unknown.
func date(fromUnix seconds: Int64) -> Date? {
    seconds > 0 ? Date(timeIntervalSince1970: TimeInterval(seconds)) : nil
}
