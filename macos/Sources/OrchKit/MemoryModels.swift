import Foundation
import Observation

/// Drives the Memory search view, and the Docs and Sources panes' scoped
/// searches.
///
/// A `paged` model (the Memory pane's) reads `key:value` filters out of the
/// text, fetches one ranked batch of `SearchQuery.maxResults` per search and
/// reveals it `query.limit` hits at a time. It never asks again for the next
/// page: a larger top_k ranks the hits again, so a re-query could reorder
/// what is already shown.
@MainActor
@Observable
public final class SearchModel {
    public var query = SearchQuery(text: "")
    public private(set) var result: SearchResult?
    public private(set) var isSearching = false
    public private(set) var lastError: String?
    /// The query `result` answers, so the view can say what it shows.
    public private(set) var answered: SearchQuery?
    /// Recent searches that answered, newest first, kept for this launch only.
    public private(set) var recent: [String] = []
    public static let recentLimit = 5
    /// The filters, chips and notes read from the last search's text.
    public private(set) var parsed = ParsedMemoryQuery()
    /// Hits scoring below this are hidden from the batch; 0 hides none.
    public var minRelevance: Float = 0
    public static let relevanceChoices: [Float] = [0, 0.2, 0.4, 0.6, 0.8]
    /// The Memory pane's mode: typed filters and one batch shown page by page.
    public let paged: Bool
    private var pages = 1

    private let api: any MemoryAPI
    private var current: Task<Void, Never>?
    /// Bumped per search; only the newest search may touch the published state,
    /// so a cancelled one finishing late cannot overwrite it.
    private var generation = 0

    public init(api: any MemoryAPI, paged: Bool = false) {
        self.api = api
        self.paged = paged
    }

    /// Runs the current query, cancelling one still in flight. An empty query
    /// clears the results instead of asking orchd.
    public func search() async {
        current?.cancel()
        generation += 1
        let mine = generation
        var q = query
        if paged {
            parsed = MemoryFilterParser.parse(query.text)
            q = parsed.applied(to: query)
            q.limit = SearchQuery.maxResults
        }
        q = q.normalized
        guard !q.text.isEmpty else {
            if paged && parsed.hasFilters {
                parsed.notes.append("Add words to search for; Browse lists every chunk these filters match.")
            }
            result = nil
            answered = nil
            lastError = nil
            isSearching = false
            return
        }
        isSearching = true
        let task = Task { [api] in
            let outcome: Result<SearchResult, any Error>
            do {
                outcome = .success(try await api.search(q))
            } catch {
                outcome = .failure(error)
            }
            guard mine == self.generation else { return }
            switch outcome {
            case .success(let found):
                self.result = found
                self.answered = q
                self.pages = 1
                self.remember(self.paged ? self.query.text.trimmingCharacters(in: .whitespacesAndNewlines) : q.text)
                self.lastError = nil
            case .failure(let error):
                self.lastError = DaemonMonitor.describe(error)
            }
            self.isSearching = false
        }
        current = task
        await task.value
    }

    private func remember(_ text: String) {
        recent.removeAll { $0.caseInsensitiveCompare(text) == .orderedSame }
        recent.insert(text, at: 0)
        if recent.count > Self.recentLimit { recent.removeLast(recent.count - Self.recentLimit) }
    }

    /// Runs a recent search again.
    public func repeatSearch(_ text: String) async {
        query.text = text
        await search()
    }

    /// Drops one typed filter from the text and searches again.
    public func remove(_ chip: MemoryFilterChip) async {
        query.text = MemoryFilterParser.removing(chip, from: query.text)
        await search()
    }

    /// Narrows the search to one source, replacing any `uri:` filter, and
    /// runs it when there are words to search for.
    public func within(source uri: String) async {
        let parsed = MemoryFilterParser.parse(query.text)
        var text = query.text
        for chip in parsed.chips.reversed() where chip.key == .uri {
            text = MemoryFilterParser.removing(chip, from: text)
        }
        guard let token = MemoryFilterParser.token(.uri, uri) else { return }
        query.text = text.isEmpty ? token : "\(text) \(token)"
        if !MemoryFilterParser.parse(query.text).text.isEmpty { await search() }
    }

    /// Drops the results and any search in flight, keeping the query.
    public func clear() {
        current?.cancel()
        generation += 1
        result = nil
        answered = nil
        lastError = nil
        isSearching = false
    }

    // MARK: Pages of the batch

    /// Hits shown per page.
    public var pageSize: Int { min(max(query.limit, 1), SearchQuery.maxResults) }

    /// The batch's hits at or above `minRelevance`.
    public var matchingHits: [SearchHit] { (result?.hits ?? []).filter { $0.score >= minRelevance } }

    /// The hits revealed so far: every one unless paged.
    public var visibleHits: [SearchHit] {
        paged ? Array(matchingHits.prefix(pages * pageSize)) : result?.hits ?? []
    }

    public var canShowMore: Bool { visibleHits.count < matchingHits.count }

    /// Reveals the next page of the batch, without asking orchd again.
    public func showMore() {
        if canShowMore { pages += 1 }
    }

    /// How many hits `minRelevance` hides.
    public var hiddenByRelevance: Int { (result?.hits.count ?? 0) - matchingHits.count }

    /// "Showing 20 of 100", saying when the ranked batch hit its cap and
    /// what the relevance floor hides.
    public var footer: String? {
        guard paged, let result, !result.hits.isEmpty else { return nil }
        return rankedBatchFooter(visible: visibleHits.count, matching: matchingHits.count, batch: result.hits.count,
                                 minRelevance: minRelevance, browseHint: true)
    }
}

/// "Showing 20 of 100" for one ranked batch shown a page at a time, saying
/// when the batch hit orchd's cap and what the relevance floor hides.
func rankedBatchFooter(visible: Int, matching: Int, batch: Int, minRelevance: Float, browseHint: Bool = false) -> String {
    var out = "Showing \(visible) of \(matching)"
    if batch >= SearchQuery.maxResults {
        out += " — ranked search returns at most \(SearchQuery.maxResults)"
        if browseHint { out += "; use Browse to see every match" } else { out += "; add words or filters to narrow it" }
    }
    let hidden = batch - matching
    if hidden > 0 {
        out += " · \(hidden) below relevance \(String(format: "%.1f", minRelevance)) hidden"
    }
    return out
}

/// Restarts subsystems from the dashboard and reports what happened.
@MainActor
@Observable
public final class RestartModel {
    /// The unit being restarted ("" for all), nil when idle.
    public private(set) var restarting: String?
    public private(set) var lastMessage: String?
    public private(set) var lastFailed = false

    private let api: any ControlAPI
    private let afterRestart: @MainActor () async -> Void

    public init(api: any ControlAPI, afterRestart: @escaping @MainActor () async -> Void = {}) {
        self.api = api
        self.afterRestart = afterRestart
    }

    /// Restarts `name`, or every unit when nil. Ignored while one is running.
    public func restart(_ name: String?) async {
        guard restarting == nil else { return }
        restarting = name ?? ""
        defer { restarting = nil }
        do {
            let done = try await api.restart(name)
            lastFailed = false
            lastMessage = done.isEmpty ? "Nothing restarted" : "Restarted \(done.joined(separator: ", "))"
        } catch {
            lastFailed = true
            lastMessage = "Restart failed: \(DaemonMonitor.describe(error))"
        }
        await afterRestart()
    }
}
