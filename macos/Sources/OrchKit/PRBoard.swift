import Foundation

/// Where a PR stands on its way to merge, for the PRs board. Cases are in
/// board order, left to right.
public enum PRLane: String, Sendable, CaseIterable, Identifiable {
    /// CI failing, changes requested, conflicts, or a review that asks for changes.
    case fix
    /// Drafts and PRs whose CI is still running.
    case flight
    /// Ready for review: waiting on a reviewer, on orch's review, or on its verdict.
    case review
    /// Ready to merge, or orch's review approves the current head.
    case ship
    /// Untouched for months and waiting on no one.
    case cold

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .fix: "Fix"
        case .flight: "In flight"
        case .review: "Review"
        case .ship: "Ship"
        case .cold: "Cold"
        }
    }

    public var blurb: String {
        switch self {
        case .fix: "CI failing, changes asked for, or conflicts"
        case .flight: "Drafts and CI still running"
        case .review: "Waiting on a reviewer or orch's review"
        case .ship: "Ready to merge or approved by orch"
        case .cold: "Untouched for months"
        }
    }

    public var symbolName: String {
        switch self {
        case .fix: "wrench.and.screwdriver"
        case .flight: "airplane"
        case .review: "eye"
        case .ship: "shippingbox"
        case .cold: "snowflake"
        }
    }
}

/// How long a PR may sit untouched before the board folds it into Cold.
public let prColdAge: TimeInterval = 180 * 86400

/// The lane item belongs in. review counts only for the current head, as the
/// review badge does, so a verdict on an older commit moves nothing.
public func prLane(_ item: GitHubItem, review: PRReview?, now: Date = .now) -> PRLane {
    // Someone waiting on the user is never cold, however old the PR.
    if item.attention == .reviewRequested { return .review }
    if let updated = item.updatedAt, now.timeIntervalSince(updated) > prColdAge { return .cold }
    let verdict = review?.state == .verdict ? review?.verdict : nil
    switch item.attention {
    case .ciFailed, .changesRequested, .conflicts: return .fix
    case .readyToMerge: return .ship
    default: break
    }
    if item.ciState == "failure" || verdict == .changes { return .fix }
    if verdict == .approve { return .ship }
    if item.isDraft || item.ciState == "pending" { return .flight }
    return .review
}

/// The PRs among items in each lane, keeping items' order within a lane.
public func prBoard(_ items: [GitHubItem], reviews: [String: PRReview], now: Date = .now) -> [PRLane: [GitHubItem]] {
    var lanes: [PRLane: [GitHubItem]] = [:]
    for item in items where item.isPR {
        lanes[prLane(item, review: reviews[item.url], now: now), default: []].append(item)
    }
    return lanes
}
