import Foundation

/// Where a timeline entry leads when clicked.
public enum ActivityTarget: Sendable, Equatable {
    case task(String)
    case pr(String)
    case docs
    case source(String)
}

/// What kind of work an entry records, for the timeline's filter.
public enum ActivityKind: String, Sendable, CaseIterable, Identifiable {
    case task, review, docs, source

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .task: "Tasks"
        case .review: "Reviews"
        case .docs: "Docs"
        case .source: "Sources"
        }
    }
}

/// How an entry turned out, which the timeline draws as its node's color.
public enum ActivityOutcome: Sendable, Equatable {
    case done, failed, running, waiting, attention
}

/// One thing orch did on its own: a task that ran, a review verdict, a docs
/// sync, a source refresh.
public struct ActivityEvent: Sendable, Equatable, Identifiable {
    public let id: String
    public let kind: ActivityKind
    public let date: Date
    public let title: String
    public let detail: String
    public let outcome: ActivityOutcome
    public let symbol: String
    public let target: ActivityTarget

    public init(id: String, kind: ActivityKind, date: Date, title: String, detail: String, outcome: ActivityOutcome,
                symbol: String, target: ActivityTarget) {
        self.id = id
        self.kind = kind
        self.date = date
        self.title = title
        self.detail = detail
        self.outcome = outcome
        self.symbol = symbol
        self.target = target
    }
}

/// Builds the Dashboard's timeline from what the app's models already hold.
/// orchd keeps no event log, so the feed is each record's latest change,
/// newest first: a task that ran twice shows once, at its last update.
public enum ActivityFeed {
    public static let defaultLimit = 60

    public static func events(tasks: [TaskRecord], reviews: [PRReview], docs: DocCatalog?, sources: [RegisteredSource],
                              limit: Int = defaultLimit) -> [ActivityEvent] {
        var events: [ActivityEvent] = []
        events += tasks.compactMap(taskEvent)
        events += reviews.compactMap(reviewEvent)
        if let docs { events += docsEvents(docs) }
        events += sources.compactMap(sourceEvent)
        // Ties break on id so equal timestamps keep a stable order between polls.
        events.sort { $0.date != $1.date ? $0.date > $1.date : $0.id < $1.id }
        return Array(events.prefix(max(limit, 0)))
    }

    /// orchd's task times are SQLite `datetime('now')`: UTC, no zone suffix.
    public static func taskDate(_ text: String) -> Date? {
        let trimmed = text.trimmingCharacters(in: .whitespaces)
        guard !trimmed.isEmpty else { return nil }
        if let date = sqliteFormatter.date(from: trimmed) { return date }
        return try? Date(trimmed, strategy: .iso8601)
    }

    private static let sqliteFormatter: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "UTC")
        f.dateFormat = "yyyy-MM-dd HH:mm:ss"
        return f
    }()

    static func taskEvent(_ task: TaskRecord) -> ActivityEvent? {
        guard let date = taskDate(task.updatedAt) else { return nil }
        let (verb, outcome): (String, ActivityOutcome) = switch task.status {
        case "complete": ("finished", .done)
        case "failed": ("failed", .failed)
        case "in_progress": ("running", .running)
        case "pending": ("queued", .waiting)
        case "blocked": ("blocked", .attention)
        case "interrupted": ("interrupted", .attention)
        case "cancelled": ("cancelled", .waiting)
        default: (task.status, .waiting)
        }
        var detail = ["\(task.agent) \(verb)"]
        if task.durationMS > 0 { detail.append(formatDuration(ms: task.durationMS)) }
        if task.costUSD > 0 { detail.append(String(format: "$%.2f", task.costUSD)) }
        if !task.prs.isEmpty { detail.append("\(task.prs.count) PR\(task.prs.count == 1 ? "" : "s")") }
        return ActivityEvent(id: "task:\(task.id)", kind: .task, date: date, title: headline(task.body, fallback: task.id),
                             detail: detail.joined(separator: " · "), outcome: outcome, symbol: "checklist",
                             target: .task(task.id))
    }

    static func reviewEvent(_ review: PRReview) -> ActivityEvent? {
        let name = review.repo.isEmpty ? review.url : "\(review.repo)#\(review.number)"
        if let verdict = review.verdict, let date = review.verdictAt ?? review.updatedAt {
            let outcome: ActivityOutcome = switch verdict {
            case .approve: .done
            case .wait: .waiting
            case .changes: .attention
            }
            var detail = "Verdict: \(verdict.label)"
            if review.posted == "approved" || review.posted == "commented" { detail += " · posted" }
            if review.posted == "failed" { detail += " · posting failed" }
            if review.posted == "superseded" { detail += " · superseded, not posted" }
            return ActivityEvent(id: "review:\(review.url)", kind: .review, date: date, title: name,
                                 detail: detail, outcome: review.posted == "failed" ? .failed : outcome,
                                 symbol: verdict.symbolName, target: .pr(review.url))
        }
        guard let state = review.state, state.isActive, let date = review.updatedAt else { return nil }
        let outcome: ActivityOutcome = switch state {
        case .reviewing: .running
        case .failed: .failed
        case .held: .attention
        default: .waiting
        }
        return ActivityEvent(id: "review:\(review.url)", kind: .review, date: date, title: name, detail: state.label,
                             outcome: outcome, symbol: "eye", target: .pr(review.url))
    }

    /// One entry for the sync (running or last finished) and one per failing
    /// repo; a sync that touches every repo would otherwise flood the feed.
    static func docsEvents(_ catalog: DocCatalog) -> [ActivityEvent] {
        var events: [ActivityEvent] = []
        if let sync = catalog.sync {
            let scope = sync.owner.isEmpty ? "all owners" : sync.owner
            if sync.running, let started = sync.startedAt {
                let progress = sync.total > 0 ? " · \(sync.done)/\(sync.total)" : ""
                events.append(ActivityEvent(id: "docs:sync", kind: .docs, date: started, title: "Docs sync",
                                            detail: "Syncing \(scope)\(progress)", outcome: .running,
                                            symbol: "arrow.triangle.2.circlepath", target: .docs))
            } else if let finished = sync.finishedAt {
                let failing = catalog.failingRepos.count
                let detail = failing > 0 ? "Synced \(scope) · \(failing) repo\(failing == 1 ? "" : "s") failed" : "Synced \(scope)"
                events.append(ActivityEvent(id: "docs:sync", kind: .docs, date: finished, title: "Docs sync", detail: detail,
                                            outcome: failing > 0 ? .attention : .done,
                                            symbol: "doc.text.magnifyingglass", target: .docs))
            }
        }
        for item in catalog.failingRepos {
            guard let date = item.repo.lastSynced ?? catalog.sync?.finishedAt else { continue }
            let cause = item.repo.error.trimmingCharacters(in: .whitespacesAndNewlines)
            events.append(ActivityEvent(id: "docs:\(item.owner)/\(item.repo.repo)", kind: .docs, date: date,
                                        title: "\(item.owner)/\(item.repo.repo)",
                                        detail: cause.isEmpty ? "Docs sync failed" : headline(cause, fallback: cause),
                                        outcome: .failed, symbol: "exclamationmark.triangle", target: .docs))
        }
        return events
    }

    static func sourceEvent(_ source: RegisteredSource) -> ActivityEvent? {
        guard let date = source.lastFetched else { return nil }
        let (detail, outcome): (String, ActivityOutcome) = switch source.status {
        case "error": ("Refresh failed", .failed)
        case "missing": ("Source missing", .attention)
        default: ("Refreshed", .done)
        }
        return ActivityEvent(id: "source:\(source.id)", kind: .source, date: date, title: source.uri, detail: detail,
                             outcome: outcome, symbol: "tray.full", target: .source(source.id))
    }

    /// The first non-blank line, cut to fit one row.
    static func headline(_ text: String, fallback: String, max: Int = 90) -> String {
        let line = text.split(whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .first { !$0.isEmpty } ?? ""
        guard !line.isEmpty else { return fallback }
        return line.count > max ? String(line.prefix(max - 1)) + "…" : line
    }

    static func formatDuration(ms: Int64) -> String {
        let seconds = ms / 1000
        if seconds < 60 { return "\(seconds)s" }
        if seconds < 3600 { return "\(seconds / 60)m \(seconds % 60)s" }
        return "\(seconds / 3600)h \(seconds % 3600 / 60)m"
    }

    /// The day heading an entry falls under.
    public static func day(_ date: Date, now: Date = .now, calendar: Calendar = .current) -> String {
        if calendar.isDate(date, inSameDayAs: now) { return "Today" }
        if let yesterday = calendar.date(byAdding: .day, value: -1, to: now), calendar.isDate(date, inSameDayAs: yesterday) {
            return "Yesterday"
        }
        return date.formatted(.dateTime.weekday(.wide).month(.abbreviated).day())
    }
}
