import Foundation

/// Why a PR needs the user, as orchd's `prs.Attention` decides it. Cases are
/// in priority order, most urgent first.
public enum Attention: String, Sendable, CaseIterable {
    case ciFailed = "ci_failed"
    case changesRequested = "changes_requested"
    case conflicts
    case reviewRequested = "review_requested"
    case readyToMerge = "ready_to_merge"

    public var label: String {
        switch self {
        case .ciFailed: "CI failing"
        case .changesRequested: "Changes requested"
        case .conflicts: "Merge conflicts"
        case .reviewRequested: "Review requested"
        case .readyToMerge: "Ready to merge"
        }
    }

    public var symbolName: String {
        switch self {
        case .ciFailed: "xmark.octagon"
        case .changesRequested: "exclamationmark.bubble"
        case .conflicts: "arrow.triangle.merge"
        case .reviewRequested: "eye"
        case .readyToMerge: "checkmark.seal"
        }
    }
}

/// One PR from orchd's babysitter cache.
public struct PullRequest: Sendable, Equatable, Identifiable {
    public var id: String { url }
    public let url: String
    public let title: String
    public let repo: String
    public let ciState: String
    public let reviewDecision: String
    public let mergeable: String
    public let isDraft: Bool
    public let reasons: [String]
    /// nil when the PR does not need the user, or orchd sent a reason this
    /// build does not know.
    public let attention: Attention?

    public init(
        url: String, title: String, repo: String, ciState: String = "", reviewDecision: String = "",
        mergeable: String = "", isDraft: Bool = false, reasons: [String] = [], attention: Attention? = nil
    ) {
        self.url = url
        self.title = title
        self.repo = repo
        self.ciState = ciState
        self.reviewDecision = reviewDecision
        self.mergeable = mergeable
        self.isDraft = isDraft
        self.reasons = reasons
        self.attention = attention
    }

    /// The PR's page, only when it is exactly `https://github.com/<owner>/<repo>/pull/<n>`.
    /// The cache is written from gh output, but the app hands this URL to the
    /// system to open, so it never opens another scheme, host, port or page.
    public var webURL: URL? {
        guard let components = URLComponents(string: url),
              components.scheme == "https",
              components.host == "github.com",
              components.port == nil,
              components.user == nil, components.password == nil,
              components.query == nil, components.fragment == nil,
              components.path.wholeMatch(of: /\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+\/pull\/[0-9]+/) != nil,
              let url = components.url
        else { return nil }
        return url
    }
}

/// orchd's PR list and when its cache was last written.
public struct PRList: Sendable, Equatable {
    public let prs: [PullRequest]
    /// nil when the babysitter has not written its cache yet.
    public let updatedAt: Date?

    public init(prs: [PullRequest], updatedAt: Date?) {
        self.prs = prs
        self.updatedAt = updatedAt
    }

    /// The PRs needing the user, keeping orchd's most-urgent-first order.
    public var needsYou: [PullRequest] { prs.filter { $0.attention != nil } }
}

/// The PRs in `current` that newly need the user: a PR that was not flagged
/// before, or is flagged for a different reason now. Nothing is new on the
/// first load (`previous == nil`), so launching the app never floods the user
/// with notifications for things already waiting.
public func newlyNeedingYou(previous: [PullRequest]?, current: [PullRequest]) -> [PullRequest] {
    guard let previous else { return [] }
    let seen = Set(previous.compactMap { pr in pr.attention.map { "\(pr.url)|\($0.rawValue)" } })
    return current.filter { pr in
        guard let attention = pr.attention else { return false }
        return !seen.contains("\(pr.url)|\(attention.rawValue)")
    }
}

/// What the menu bar shows: an unhealthy daemon outranks PRs, because the PR
/// list is stale while orchd is down or degraded.
public struct TrayIcon: Equatable, Sendable {
    public let symbolName: String
    /// The badge, or nil for none.
    public let count: Int?

    public init(health: Health, needsYou: Int) {
        let badge = needsYou > 0 ? needsYou : nil
        switch health {
        case .healthy:
            symbolName = badge == nil ? health.symbolName : "bell.badge"
            count = badge
        case .degraded:
            symbolName = health.symbolName
            count = badge
        case .unknown, .down:
            symbolName = health.symbolName
            count = nil
        }
    }
}

/// How a batch of newly flagged PRs is announced: one notification each, or a
/// single summary once there are more than `limit`, so a burst (a sweep after
/// a long sleep, a flood of review requests) never stacks a screenful of
/// banners.
public enum NotificationPlan: Equatable, Sendable {
    case each([PullRequest])
    case summary(count: Int)

    public static let defaultLimit = 3

    public init(_ prs: [PullRequest], limit: Int = defaultLimit) {
        self = prs.count > limit ? .summary(count: prs.count) : .each(prs)
    }
}
