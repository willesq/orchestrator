import Foundation
import Observation

/// Drives the Spaces pane's Activity page: the pinned sessions, and the
/// chosen one's command log with its directory moves and refusals, newest
/// first.
@MainActor
@Observable
public final class SpacesActivityModel {
    public private(set) var sessions: [ActivitySession] = []
    public private(set) var selected: String?
    /// The chosen session's log, newest first.
    public private(set) var rows: [ActivityRow] = []
    /// The log has records past the ones read.
    public private(set) var more = false
    public private(set) var lastError: String?
    public private(set) var isLoading = false
    public var filter: ActivityFilter = .all

    /// Pages read at a time; each holds up to `pageSize` records.
    public static let pagesPerRead = 5
    public static let pageSize: Int32 = 1000
    /// Most rows kept; the oldest go first, so a very long log cannot grow
    /// without bound.
    public static let maxRows = 20_000

    private let api: any SpacesActivityAPI
    private let home: String
    private var lastSeq: Int64 = 0
    private var generation = 0

    public init(api: any SpacesActivityAPI, home: String = NSHomeDirectory()) {
        self.api = api
        self.home = home
    }

    public var visibleRows: [ActivityRow] { rows.filter(filter.admits) }

    public var selectedSession: ActivitySession? { sessions.first { $0.id == selected } }

    /// Reads the sessions, keeps the chosen one if it is still listed (else
    /// picks the most recent), and reads its log again.
    public func load() async {
        generation += 1
        let mine = generation
        isLoading = true
        defer { if mine == generation { isLoading = false } }
        let pins: [PinnedSession]
        do {
            pins = try await api.listSessions(includeEnded: true)
        } catch {
            guard mine == generation else { return }
            lastError = spacesRefusal(error)
            return
        }
        guard mine == generation else { return }
        // A harness process with no pin has no session log to name here
        // (its process chain has no owner field in ListSessions).
        sessions = pins.filter { !$0.sessionID.isEmpty }
            .sorted { ($0.lastActivity ?? .distantPast) > ($1.lastActivity ?? .distantPast) }
            .map { Self.session($0, home: home) }
        if selected == nil || !sessions.contains(where: { $0.id == selected }) { selected = sessions.first?.id }
        lastError = nil
        await readLog(fresh: true)
    }

    public func select(_ id: String) async {
        guard id != selected, sessions.contains(where: { $0.id == id }) else { return }
        selected = id
        await readLog(fresh: true)
    }

    /// Reads the next records of the chosen session's log.
    public func loadMore() async {
        guard more else { return }
        await readLog(fresh: false)
    }

    private func readLog(fresh: Bool) async {
        generation += 1
        let mine = generation
        guard let owner = selected else {
            rows = []
            more = false
            return
        }
        if fresh {
            rows = []
            lastSeq = 0
            more = false
        }
        isLoading = true
        defer { if mine == generation { isLoading = false } }
        var read: [ActivityRow] = []
        var after = lastSeq
        var hasMore = false
        do {
            for _ in 0..<Self.pagesPerRead {
                let page = try await api.toolEvents(owner: owner, afterSeq: after, limit: Self.pageSize)
                read += page.events.map { ActivityRow.build($0, home: home) }
                after = page.events.last?.seq ?? after
                hasMore = page.more && !page.events.isEmpty
                if !hasMore { break }
            }
        } catch {
            guard mine == generation else { return }
            lastError = spacesRefusal(error)
            return
        }
        guard mine == generation else { return }
        rows = Array((read.reversed() + rows).prefix(Self.maxRows))
        lastSeq = after
        more = hasMore
        lastError = nil
    }

    static func session(_ s: PinnedSession, home: String) -> ActivitySession {
        ActivitySession(id: s.sessionID,
                        title: SpacesOverview.harnessName(s.harness) + " · " + displaySafe(String(s.sessionID.prefix(8))),
                        space: displaySafe(s.space), directory: SpacesOverview.abbreviate(s.firstCwd, home: home), state: displaySafe(s.state),
                        toolEvents: s.toolEvents, refusals: s.refusals, lastActivity: s.lastActivity)
    }
}
