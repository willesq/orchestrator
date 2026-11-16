import Foundation

/// A filter typed into the Memory search field, such as `type:repo_doc`.
public enum MemoryFilterKey: String, Sendable, CaseIterable {
    case type, repo, tag, collection, uri, scope, since, until

    /// What a chip calls it.
    public var label: String {
        switch self {
        case .type: "Type"
        case .repo: "Repo"
        case .tag: "Tag"
        case .collection: "Collection"
        case .uri: "Source"
        case .scope: "Scope"
        case .since: "Since"
        case .until: "Until"
        }
    }
}

/// One filter taken from the text, shown as a removable chip.
public struct MemoryFilterChip: Sendable, Equatable, Identifiable {
    /// The token's place in the text, which removing the chip drops.
    public let id: Int
    public let key: MemoryFilterKey
    /// The value as typed, without quotes.
    public let value: String

    public var label: String { "\(key.label): \(displaySafe(value))" }
}

/// What the parser made of the search field: the words left to search for,
/// the filters, a chip per filter, and notes on values it could not use.
public struct ParsedMemoryQuery: Sendable, Equatable {
    public var text = ""
    public var sourceType = ""
    public var scope = ""
    public var sourceURI = ""
    /// `tag:` and `repo:` values (`repo:x` is the tag `repo:x`); a chunk
    /// matches only when it carries every one of them, so each narrows.
    public var tags: [String] = []
    public var collections: [String] = []
    public var since: Date?
    public var until: Date?
    public var chips: [MemoryFilterChip] = []
    /// Why a filter was left out, e.g. a date that does not parse.
    public var notes: [String] = []

    public init() {}

    /// Whether any filter was given.
    public var hasFilters: Bool { !chips.isEmpty }

    /// `query` with these filters laid over the menus' choices: a typed
    /// type, source or collection replaces the menu's; tags and dates add.
    public func applied(to query: SearchQuery) -> SearchQuery {
        var q = query
        q.text = text
        if !sourceType.isEmpty { q.sourceType = sourceType }
        if !scope.isEmpty { q.scope = scope }
        if !sourceURI.isEmpty { q.sourceURI = sourceURI }
        if !collections.isEmpty { q.collections = collections }
        q.allTags += tags
        q.since = since ?? q.since
        q.until = until ?? q.until
        return q
    }

    /// The same for a browse, which has no text.
    public func applied(to query: BrowseQuery) -> BrowseQuery {
        var q = query
        if !sourceType.isEmpty { q.sourceType = sourceType }
        if !scope.isEmpty { q.scope = scope }
        if !sourceURI.isEmpty { q.sourceURI = sourceURI }
        if !collections.isEmpty { q.collections = collections }
        q.allTags += tags
        q.since = since ?? q.since
        q.until = until ?? q.until
        return q
    }
}

/// Reads `key:value` filters out of search text. Unknown keys (and a URL's
/// `https:`) stay part of the text, as do quoted phrases; a value that
/// cannot be used is dropped with a note rather than failing the search.
public enum MemoryFilterParser {
    public static func parse(_ raw: String, now: Date = Date(), calendar: Calendar = .current) -> ParsedMemoryQuery {
        var out = ParsedMemoryQuery()
        var words: [String] = []
        for (index, token) in tokens(raw).enumerated() {
            guard let (key, value) = filter(token) else {
                words.append(token)
                continue
            }
            guard !value.isEmpty else {
                out.notes.append("\(key.rawValue): needs a value, so it was left out")
                continue
            }
            switch key {
            case .type: replace(&out.sourceType, with: value, key: key, notes: &out.notes)
            case .scope: replace(&out.scope, with: value, key: key, notes: &out.notes)
            case .uri: replace(&out.sourceURI, with: value, key: key, notes: &out.notes)
            case .repo: out.tags.append("repo:\(value)")
            case .tag: out.tags.append(value)
            case .collection: out.collections.append(value)
            case .since, .until:
                guard let date = date(value, end: key == .until, now: now, calendar: calendar) else {
                    out.notes.append("\(key.rawValue):\(displaySafe(value)) is not a date, so it was left out; "
                        + "try 7d, 24h, 2w, today, yesterday or 2026-11-01")
                    continue
                }
                if key == .since { out.since = date } else { out.until = date }
            }
            out.chips.append(MemoryFilterChip(id: index, key: key, value: value))
        }
        // A later single-valued filter replaced an earlier one: keep only the
        // chip that applies.
        out.chips = out.chips.enumerated().filter { i, chip in
            [.type, .scope, .uri, .since, .until].contains(chip.key) == false
                || !out.chips[(i + 1)...].contains { $0.key == chip.key }
        }.map(\.element)
        if let since = out.since, let until = out.until, since > until {
            out.notes.append("since is after until, so nothing can match")
        }
        out.text = words.joined(separator: " ")
        return out
    }

    /// `raw` without the token a chip came from, for removing that filter.
    public static func removing(_ chip: MemoryFilterChip, from raw: String) -> String {
        tokens(raw).enumerated().filter { $0.offset != chip.id }.map(\.element).joined(separator: " ")
    }

    /// A filter token for `value`, quoted when it holds spaces; nil when it
    /// holds a double quote, which no token can carry.
    public static func token(_ key: MemoryFilterKey, _ value: String) -> String? {
        guard !value.isEmpty, !value.contains("\"") else { return nil }
        return value.contains(where: \.isWhitespace) ? "\(key.rawValue):\"\(value)\"" : "\(key.rawValue):\(value)"
    }

    /// Splits on whitespace outside double quotes, so `"exact phrase"` and
    /// `uri:"a b"` are each one token.
    static func tokens(_ raw: String) -> [String] {
        var out: [String] = []
        var current = ""
        var quoted = false
        for c in raw {
            if c == "\"" { quoted.toggle() }
            if c.isWhitespace && !quoted {
                if !current.isEmpty { out.append(current) }
                current = ""
            } else {
                current.append(c)
            }
        }
        if !current.isEmpty { out.append(current) }
        return out
    }

    /// The key and unquoted value of a `key:value` token with a known key.
    private static func filter(_ token: String) -> (MemoryFilterKey, String)? {
        guard !token.hasPrefix("\""), let colon = token.firstIndex(of: ":"),
              let key = MemoryFilterKey(rawValue: token[..<colon].lowercased())
        else { return nil }
        var value = String(token[token.index(after: colon)...])
        if value.count >= 2, value.hasPrefix("\""), value.hasSuffix("\"") { value = String(value.dropFirst().dropLast()) }
        return (key, value.trimmingCharacters(in: .whitespaces))
    }

    private static func replace(_ field: inout String, with value: String, key: MemoryFilterKey, notes: inout [String]) {
        if !field.isEmpty && field != value {
            notes.append("only one \(key.rawValue): filter applies, so \(displaySafe(value)) replaced \(displaySafe(field))")
        }
        field = value
    }

    /// An age (`7d`, `24h`, `2w`), `today`, `yesterday`, or an ISO date or
    /// time. A day as an upper bound means the end of that day.
    static func date(_ value: String, end: Bool, now: Date, calendar: Calendar) -> Date? {
        let lower = value.lowercased()
        let day: Date?
        switch lower {
        case "today": day = calendar.startOfDay(for: now)
        case "yesterday": day = calendar.date(byAdding: .day, value: -1, to: calendar.startOfDay(for: now))
        default: day = isoDay(lower, calendar: calendar)
        }
        if let day {
            return end ? calendar.date(byAdding: DateComponents(day: 1, second: -1), to: day) : day
        }
        if let age = age(lower) { return now.addingTimeInterval(-age) }
        return ISO8601DateFormatter().date(from: value)
    }

    private static func age(_ value: String) -> TimeInterval? {
        guard let unit = value.last, let n = Int(value.dropLast()), n >= 0, n <= 100_000 else { return nil }
        switch unit {
        case "h": return TimeInterval(n) * 3600
        case "d": return TimeInterval(n) * 86400
        case "w": return TimeInterval(n) * 7 * 86400
        default: return nil
        }
    }

    private static func isoDay(_ value: String, calendar: Calendar) -> Date? {
        let parts = value.split(separator: "-", omittingEmptySubsequences: false)
        guard parts.count == 3, parts[0].count == 4, parts[1].count == 2, parts[2].count == 2,
              let y = Int(parts[0]), let m = Int(parts[1]), let d = Int(parts[2]),
              let date = calendar.date(from: DateComponents(year: y, month: m, day: d)),
              calendar.component(.month, from: date) == m, calendar.component(.day, from: date) == d
        else { return nil }
        return date
    }
}
