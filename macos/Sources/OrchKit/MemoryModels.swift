import Foundation
import Observation

/// Drives the Memory search view.
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

    private let api: any MemoryAPI
    private var current: Task<Void, Never>?
    /// Bumped per search; only the newest search may touch the published state,
    /// so a cancelled one finishing late cannot overwrite it.
    private var generation = 0

    public init(api: any MemoryAPI) {
        self.api = api
    }

    /// Runs the current query, cancelling one still in flight. An empty query
    /// clears the results instead of asking orchd.
    public func search() async {
        current?.cancel()
        generation += 1
        let mine = generation
        let q = query.normalized
        guard !q.text.isEmpty else {
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
                self.remember(q.text)
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

    /// Drops the results and any search in flight, keeping the query.
    public func clear() {
        current?.cancel()
        generation += 1
        result = nil
        answered = nil
        lastError = nil
        isSearching = false
    }
}

/// Drives the Memory sources list.
@MainActor
@Observable
public final class SourcesModel {
    public static let pageSize = 200

    public var typeFilter = ""
    public private(set) var list: SourceList?
    public private(set) var lastError: String?

    private let api: any MemoryAPI

    public init(api: any MemoryAPI) {
        self.api = api
    }

    public func refresh() async {
        do {
            let fresh = try await api.listSources(type: typeFilter.trimmingCharacters(in: .whitespaces), limit: Self.pageSize)
            list = fresh
            seenTypes.formUnion(fresh.sources.map(\.sourceType).filter { !$0.isEmpty })
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Every source type seen in any listing, so filtering to one type keeps
    /// the others in the menu.
    private var seenTypes: Set<String> = []

    /// Source types seen so far, for the filter menus.
    public var knownTypes: [String] { seenTypes.sorted() }
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
