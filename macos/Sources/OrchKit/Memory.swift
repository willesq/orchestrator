import Foundation

/// How orchd ranks a search: both semantic and keyword (hybrid), meaning only
/// (dense), or whatever `rag.search.mode` configures.
public enum SearchMode: String, Sendable, CaseIterable, Identifiable {
    case configured = ""
    case hybrid
    case dense

    public var id: String { rawValue }

    /// What each choice does, for its tooltip.
    public var help: String {
        switch self {
        case .configured: "Search the way rag.search.mode in your config says"
        case .hybrid: "Match exact words and meaning together"
        case .dense: "Match by meaning only, ignoring exact words"
        }
    }

    public var label: String {
        switch self {
        case .configured: "As configured"
        case .hybrid: "Words and meaning"
        case .dense: "Meaning only"
        }
    }
}

public struct SearchQuery: Sendable, Equatable {
    /// orchd rejects anything above this.
    public static let maxResults = 100
    /// The result counts the search view offers.
    public static let limitChoices = [5, 10, 20, 50, 100]

    public var text: String
    public var limit: Int
    public var sourceType: String
    public var scope: String
    public var mode: SearchMode
    /// Only this source's content, when set.
    public var sourceURI: String
    /// Only content carrying every one of these tags, e.g. one repo's docs.
    public var allTags: [String]

    public init(text: String, limit: Int = 10, sourceType: String = "", scope: String = "", mode: SearchMode = .configured,
                sourceURI: String = "", allTags: [String] = []) {
        self.text = text
        self.limit = limit
        self.sourceType = sourceType
        self.scope = scope
        self.mode = mode
        self.sourceURI = sourceURI
        self.allTags = allTags
    }

    /// The query as sent: trimmed text, trimmed filters, a limit in 1...maxResults.
    public var normalized: SearchQuery {
        SearchQuery(
            text: text.trimmingCharacters(in: .whitespacesAndNewlines),
            limit: min(max(limit, 1), Self.maxResults),
            sourceType: sourceType.trimmingCharacters(in: .whitespaces),
            scope: scope.trimmingCharacters(in: .whitespaces),
            mode: mode,
            sourceURI: sourceURI,
            allTags: allTags.filter { !$0.isEmpty }
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

    /// A short name for where the hit came from: a file's name, a page's
    /// host and last path part, or "Session 1a2b3c4d · transcript".
    public var sourceTitle: String {
        guard let components = URLComponents(string: sourceURI), let scheme = components.scheme else {
            return displaySafe(sourceURI)
        }
        let parts = components.path.split(separator: "/").map(String.init)
        if scheme == "session" {
            let id = String((components.host ?? parts.first ?? "").prefix(8))
            let kind = parts.last ?? ""
            return displaySafe(kind.isEmpty ? "Session \(id)" : "Session \(id) · \(kind)")
        }
        if let last = parts.last { return displaySafe(last) }
        return displaySafe(components.host ?? sourceURI)
    }

    /// The text with surrounding space trimmed and runs of blank lines
    /// collapsed to one, for the collapsed preview; expanded, the text shows
    /// verbatim.
    public var preview: String {
        var out: [Substring] = []
        var blank = false
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let isBlank = line.allSatisfy(\.isWhitespace)
            if isBlank && (blank || out.isEmpty) { continue }
            blank = isBlank
            out.append(isBlank ? "" : line)
        }
        return out.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Whether the preview is long enough that four lines may cut it.
    public var isLong: Bool { preview.count > 320 || preview.split(separator: "\n").count > 4 }
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
