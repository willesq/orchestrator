import Foundation

/// Fuzzy matching for the command palette: a query matches text when its
/// characters appear in order, case-insensitively. Matches at word starts and
/// in unbroken runs score higher, so "nt" finds "New task" before "Sentinel".
public enum PaletteMatcher {
    /// The match's score, higher is better, or nil when the query does not match.
    /// An empty or blank query matches everything with a score of 0.
    public static func score(_ query: String, in text: String) -> Int? {
        let needle = Array(query.lowercased().filter { !$0.isWhitespace })
        guard !needle.isEmpty else { return 0 }
        let hay = Array(text.lowercased())
        var score = 0
        var qi = 0
        var previous: Int?
        for (i, ch) in hay.enumerated() where qi < needle.count && ch == needle[qi] {
            score += 1
            if i == 0 || !(hay[i - 1].isLetter || hay[i - 1].isNumber) { score += 8 }
            if let p = previous {
                score += p == i - 1 ? 5 : -min(i - p - 1, 4)
            }
            previous = i
            qi += 1
        }
        guard qi == needle.count else { return nil }
        if hay.starts(with: needle) { score += 20 }
        return score
    }

    /// The items that match the query, best first; ties keep their given order,
    /// so callers list what matters most first. At most `limit` are returned.
    public static func rank<T>(_ items: [T], query: String, limit: Int = .max, text: (T) -> String) -> [T] {
        let scored = items.enumerated().compactMap { offset, item in
            score(query, in: text(item)).map { (item: item, score: $0, offset: offset) }
        }
        return scored
            .sorted { $0.score != $1.score ? $0.score > $1.score : $0.offset < $1.offset }
            .prefix(limit)
            .map(\.item)
    }

    /// The items gathered by group, each group where its first item stood, so
    /// a ranked list keeps its best group first without repeating headings.
    public static func grouped<T>(_ items: [T], by group: (T) -> String) -> [T] {
        var order: [String] = []
        var members: [String: [T]] = [:]
        for item in items {
            let key = group(item)
            if members[key] == nil { order.append(key) }
            members[key, default: []].append(item)
        }
        return order.flatMap { members[$0] ?? [] }
    }
}
