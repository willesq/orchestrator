import Foundation

/// How a hit's inspector shows it: its fields laid out, or its raw JSON.
public enum HitInspectorView: String, CaseIterable, Sendable {
    case fields, json

    /// The defaults key the choice is kept under, so it holds across hits
    /// and launches.
    public static let storageKey = "MemoryInspectorView"

    public var title: String {
        switch self {
        case .fields: "Fields"
        case .json: "JSON"
        }
    }
}

/// Where a hit's source can be opened from the app: a local file revealed in
/// the Finder, or a plain web page.
public enum SourceLink: Sendable, Equatable {
    case file(URL)
    case web(URL)

    /// A link for `uri` only when it is a local `file://` path or a plain
    /// `http(s)` address; any other scheme is never opened, since the
    /// address is stored, untrusted data.
    public init?(uri: String) {
        if let web = webSourceURL(uri) {
            self = .web(web)
            return
        }
        guard let url = URL(string: uri), url.scheme?.lowercased() == "file", url.isFileURL,
              url.host.map({ $0.isEmpty || $0 == "localhost" }) ?? true, url.path.hasPrefix("/"), isPlain(url.path)
        else { return nil }
        self = .file(url)
    }
}

extension SearchHit {
    /// The repo a hit came from, from its `repo:` tag; empty when it has none.
    public var repo: String {
        tags.first { $0.hasPrefix("repo:") }.map { String($0.dropFirst("repo:".count)) } ?? ""
    }

    /// The source's short name with its repo, so two README.md hits say
    /// which repo each is from: "README.md · revok".
    public var displayTitle: String {
        let title = sourceTitle
        let repo = displaySafe(repo)
        guard !repo.isEmpty, !title.localizedCaseInsensitiveContains(repo) else { return title }
        return "\(title) · \(repo)"
    }

    /// Where the source can be opened, when its address allows it.
    public var sourceLink: SourceLink? { SourceLink(uri: sourceURI) }

    /// Every field as pretty-printed JSON with sorted keys, dates in
    /// ISO 8601, for Copy as JSON.
    public var json: String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        encoder.dateEncodingStrategy = .iso8601
        guard let data = try? encoder.encode(HitJSON(self)) else { return "{}" }
        return String(decoding: data, as: UTF8.self)
    }
}

/// SearchHit's wire-like JSON shape, with orchd's field names.
private struct HitJSON: Encodable {
    let text: String
    let source_uri: String
    let source_type: String
    let scope: String
    let score: Float
    let keyword_score: Float
    let exact: Bool
    let tags: [String]
    let started_at: Date?
    let ended_at: Date?
    let ingested_at: Date?
    let heading: String
    let trust: String
    let space: String
    let tenant: String
    let collection: String
    let point_id: String
    let chunk_index: Int?

    init(_ h: SearchHit) {
        text = h.text
        source_uri = h.sourceURI
        source_type = h.sourceType
        scope = h.scope
        score = h.score
        keyword_score = h.keywordScore
        exact = h.exact
        tags = h.tags
        started_at = h.startedAt
        ended_at = h.endedAt
        ingested_at = h.ingestedAt
        heading = h.heading
        trust = h.trust
        space = h.space
        tenant = h.tenant
        collection = h.collection
        point_id = h.pointID
        chunk_index = h.chunkIndex
    }

    // Reason: absent dates and chunk index are written as null, not left
    // out, so the copy always names every field.
    func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(text, forKey: .text)
        try c.encode(source_uri, forKey: .source_uri)
        try c.encode(source_type, forKey: .source_type)
        try c.encode(scope, forKey: .scope)
        try c.encode(score, forKey: .score)
        try c.encode(keyword_score, forKey: .keyword_score)
        try c.encode(exact, forKey: .exact)
        try c.encode(tags, forKey: .tags)
        try c.encode(started_at, forKey: .started_at)
        try c.encode(ended_at, forKey: .ended_at)
        try c.encode(ingested_at, forKey: .ingested_at)
        try c.encode(heading, forKey: .heading)
        try c.encode(trust, forKey: .trust)
        try c.encode(space, forKey: .space)
        try c.encode(tenant, forKey: .tenant)
        try c.encode(collection, forKey: .collection)
        try c.encode(point_id, forKey: .point_id)
        try c.encode(chunk_index, forKey: .chunk_index)
    }

    enum CodingKeys: String, CodingKey {
        case text, source_uri, source_type, scope, score, keyword_score, exact, tags, started_at, ended_at, ingested_at
        case heading, trust, space, tenant, collection, point_id, chunk_index
    }
}

extension SearchHit {
    /// What the hit's scores mean, for the score's tooltip.
    public var scoreHelp: String {
        var lines = ["Relevance \(String(format: "%.2f", score)): how well it matches the whole query, as ranked. "
            + "Higher is better; compare only within one search."]
        lines.append(keywordScore > 0
            ? "Keyword \(String(format: "%.2f", keywordScore)): the keyword (BM25) score of the words that matched."
            : "Keyword 0: no word matched; it was found by meaning.")
        if exact { lines.append("Exact: contains, verbatim, a code identifier from the query.") }
        return lines.joined(separator: "\n")
    }
}
