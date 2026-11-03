import Foundation

/// What orchd may post on GitHub for a watched PR, as `reviews.Mode*` names it.
public enum PRReviewMode: String, Sendable, CaseIterable, Identifiable {
    /// Post nothing; the user decides on each verdict.
    case verdict
    /// Approve, with the drafted comment as its body, when the verdict is approve.
    case approve
    /// Post the drafted comment when the verdict is not approve, so the author
    /// can fix what the review found.
    case feedback
    /// Both approve and feedback.
    case auto
    /// Post the drafted comment once, now; the user's own call on a verdict.
    case comment

    public var id: String { rawValue }

    /// The standing mode for the two permissions the user grants.
    public init(mayApprove: Bool, mayComment: Bool) {
        switch (mayApprove, mayComment) {
        case (true, true): self = .auto
        case (true, false): self = .approve
        case (false, true): self = .feedback
        case (false, false): self = .verdict
        }
    }

    /// Whether orch may approve on its own.
    public var mayApprove: Bool { self == .approve || self == .auto }
    /// Whether orch may post its suggestions on its own when it does not approve.
    public var mayComment: Bool { self == .feedback || self == .auto }

    public var explanation: String {
        switch self {
        case .verdict: "Reviews it when it is ready and tells you the verdict. Nothing is posted unless you choose to."
        case .approve: "Approves on GitHub when the verdict is approve, with its comment if it has one. Otherwise asks you."
        case .feedback: "Posts its suggestions as a review comment when it does not approve, so the author can fix them; the next push is reviewed again. Never approves."
        case .auto: "Approves when the verdict is approve; otherwise posts its suggestions for the author. Each push is reviewed again."
        case .comment: "Posts the drafted comment once, now, then posts nothing more on its own."
        }
    }
}

/// How far a watched PR's review has come, for the progress row.
public enum PRReviewStage: Int, Sendable, CaseIterable, Comparable {
    case waiting, reviewing, verdict, posted

    public var label: String {
        switch self {
        case .waiting: "Waiting until ready"
        case .reviewing: "Reviewing"
        case .verdict: "Verdict"
        case .posted: "Posted"
        }
    }

    public static func < (a: Self, b: Self) -> Bool { a.rawValue < b.rawValue }
}

/// Where a watched PR's review stands.
public enum PRReviewState: String, Sendable {
    case waiting, held, reviewing, verdict, failed, closed, stopped

    public var label: String {
        switch self {
        case .waiting: "Waiting"
        case .held: "Waiting for your go-ahead"
        case .reviewing: "Reviewing"
        case .verdict: "Verdict"
        case .failed: "Review failed"
        case .closed: "Closed"
        case .stopped: "Stopped"
        }
    }

    /// Whether orchd still works on it.
    public var isActive: Bool { self != .closed && self != .stopped }
}

/// A review task's verdict.
public enum PRVerdict: String, Sendable {
    case approve, wait, changes

    public var label: String {
        switch self {
        case .approve: "Approve"
        case .wait: "Wait"
        case .changes: "Changes needed"
        }
    }

    public var symbolName: String {
        switch self {
        case .approve: "checkmark.seal"
        case .wait: "hourglass"
        case .changes: "exclamationmark.bubble"
        }
    }
}

/// One PR orchd watches for review. Reason and comment come from a review of
/// other people's code: shown verbatim, never as Markdown.
public struct PRReview: Sendable, Equatable, Identifiable {
    public var id: String { url }
    public let url: String
    public let repo: String
    public let number: Int
    public let mode: PRReviewMode
    /// nil when orchd sent a state this build does not know.
    public let state: PRReviewState?
    public let headSHA: String
    public let pendingSHA: String
    public let taskID: String
    public let verdict: PRVerdict?
    public let reason: String
    public let comment: String
    /// The user's own notes for this PR's reviews.
    public let instructions: String
    /// The next ready commit is reviewed although its author is not trusted.
    public let goAhead: Bool
    /// The review's answer to the notes, and its full text; never posted.
    public let forUser: String
    public let report: String
    /// approved | commented | failed, for headSHA; empty when nothing.
    public let posted: String
    public let note: String
    public let verdictAt: Date?
    public let updatedAt: Date?
    /// When orchd last read the PR from GitHub, and when it reads it next.
    public let checkedAt: Date?
    public let nextCheckAt: Date?

    public init(url: String, repo: String = "", number: Int = 0, mode: PRReviewMode = .verdict,
                state: PRReviewState? = .waiting, headSHA: String = "", pendingSHA: String = "", taskID: String = "",
                verdict: PRVerdict? = nil, reason: String = "", comment: String = "", instructions: String = "",
                goAhead: Bool = false, forUser: String = "", report: String = "", posted: String = "",
                note: String = "", verdictAt: Date? = nil, updatedAt: Date? = nil, checkedAt: Date? = nil,
                nextCheckAt: Date? = nil) {
        self.url = url
        self.repo = repo
        self.number = number
        self.mode = mode
        self.state = state
        self.headSHA = headSHA
        self.pendingSHA = pendingSHA
        self.taskID = taskID
        self.verdict = verdict
        self.reason = reason
        self.comment = comment
        self.instructions = instructions
        self.goAhead = goAhead
        self.forUser = forUser
        self.report = report
        self.posted = posted
        self.note = note
        self.verdictAt = verdictAt
        self.updatedAt = updatedAt
        self.checkedAt = checkedAt
        self.nextCheckAt = nextCheckAt
    }

    /// The stage the progress row marks as current.
    public var stage: PRReviewStage {
        switch state {
        case .reviewing, .failed: .reviewing
        case .verdict: posted == "approved" || posted == "commented" ? .posted : .verdict
        default: .waiting
        }
    }

    /// The first seven characters of a commit, for display.
    public static func short(_ sha: String) -> String { String(sha.prefix(7)) }

    /// What orchd posted, in words, or nil.
    public var postedLabel: String? {
        switch posted {
        case "approved": "Approved on GitHub at \(Self.short(headSHA))"
        case "commented": "Comment posted at \(Self.short(headSHA))"
        case "failed": "Posting failed"
        default: nil
        }
    }

    /// Whether the verdict is for an older commit than the one now under
    /// review or waiting.
    public var verdictIsStale: Bool { verdict != nil && state != .verdict }
}

/// A PR's metadata from GitHub. Title, body, labels and paths are other
/// people's text: shown verbatim, never as Markdown.
public struct PRDetail: Sendable, Equatable {
    public struct Reviewer: Sendable, Equatable, Hashable { public let login: String; public let state: String }
    public struct Check: Sendable, Equatable, Hashable { public let name: String; public let result: String }
    public struct File: Sendable, Equatable, Hashable {
        public let path: String
        public let additions: Int
        public let deletions: Int
    }

    public let url: String
    public let title: String
    public let body: String
    public let author: String
    public let state: String
    public let isDraft: Bool
    public let baseRef: String
    public let headRef: String
    public let headSHA: String
    public let additions: Int
    public let deletions: Int
    public let changedFiles: Int
    public let createdAt: Date?
    public let updatedAt: Date?
    public let mergeable: String
    public let reviewDecision: String
    public let ciState: String
    public let labels: [String]
    public let requestedReviewers: [String]
    public let reviews: [Reviewer]
    public let checks: [Check]
    public let files: [File]
    public let fetchedAt: Date?

    public init(url: String, title: String = "", body: String = "", author: String = "", state: String = "OPEN",
                isDraft: Bool = false, baseRef: String = "", headRef: String = "", headSHA: String = "",
                additions: Int = 0, deletions: Int = 0, changedFiles: Int = 0, createdAt: Date? = nil,
                updatedAt: Date? = nil, mergeable: String = "", reviewDecision: String = "", ciState: String = "",
                labels: [String] = [], requestedReviewers: [String] = [], reviews: [Reviewer] = [],
                checks: [Check] = [], files: [File] = [], fetchedAt: Date? = nil) {
        self.url = url
        self.title = title
        self.body = body
        self.author = author
        self.state = state
        self.isDraft = isDraft
        self.baseRef = baseRef
        self.headRef = headRef
        self.headSHA = headSHA
        self.additions = additions
        self.deletions = deletions
        self.changedFiles = changedFiles
        self.createdAt = createdAt
        self.updatedAt = updatedAt
        self.mergeable = mergeable
        self.reviewDecision = reviewDecision
        self.ciState = ciState
        self.labels = labels
        self.requestedReviewers = requestedReviewers
        self.reviews = reviews
        self.checks = checks
        self.files = files
        self.fetchedAt = fetchedAt
    }

    /// Checks grouped by outcome, for a one-line summary. Skipped includes neutral.
    public var checkSummary: (failing: Int, running: Int, passing: Int, skipped: Int) {
        var out = (failing: 0, running: 0, passing: 0, skipped: 0)
        for c in checks {
            switch c.result {
            case "success": out.passing += 1
            case "neutral", "skipped": out.skipped += 1
            case "failure", "error", "timed_out", "cancelled", "action_required", "startup_failure": out.failing += 1
            default: out.running += 1
            }
        }
        return out
    }
}

/// PR review watches, through orchd.
public protocol PRReviewAPI: Sendable {
    func prReviews() async throws -> [PRReview]
    /// Starts watching url, or changes its mode; a non-nil comment replaces
    /// the drafted one.
    func watchPRReview(url: String, mode: PRReviewMode, comment: String?, instructions: String?,
                       reviewAnyway: Bool) async throws -> PRReview
    func stopPRReview(url: String, forget: Bool) async throws
    func prDetail(url: String, refresh: Bool) async throws -> PRDetail
}

/// The reviews in `current` with a verdict that is new since `previous`: one
/// that had none, or a different verdict time. Nothing is new on the first
/// load (`previous == nil`), so launching the app does not repeat old news.
public func newVerdicts(previous: [PRReview]?, current: [PRReview]) -> [PRReview] {
    guard let previous else { return [] }
    let seen = Dictionary(previous.map { ($0.url, $0.verdictAt) }, uniquingKeysWith: { a, _ in a })
    return current.filter { r in
        guard r.verdict != nil, r.state == .verdict, let at = r.verdictAt else { return false }
        return seen[r.url] != .some(at)
    }
}

/// The reviews in `current` newly held for the user's go-ahead. Nothing is new
/// on the first load.
public func newlyHeld(previous: [PRReview]?, current: [PRReview]) -> [PRReview] {
    guard let previous else { return [] }
    let held = Set(previous.filter { $0.state == .held }.map(\.url))
    return current.filter { $0.state == .held && !held.contains($0.url) }
}

/// How the PRs list narrows by the user's part in each PR.
public enum PRRoleFilter: String, Sendable, CaseIterable, Identifiable {
    case all, mine, toReview, other

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .all: "Any role"
        case .mine: "Mine"
        case .toReview: "Review requested"
        case .other: "Involved"
        }
    }

    func matches(_ item: GitHubItem) -> Bool {
        switch self {
        case .all: true
        case .mine: item.reasons.contains(.authored)
        case .toReview: item.reasons.contains(.reviewRequested)
        case .other: !item.reasons.contains(.authored) && !item.reasons.contains(.reviewRequested)
        }
    }
}

/// How the PRs list narrows by each PR's state.
public enum PRStateFilter: String, Sendable, CaseIterable, Identifiable {
    case open, needsAction, draft, ciFailing, watched, verdicts

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .open: "All open"
        case .needsAction: "Needs action"
        case .draft: "Drafts"
        case .ciFailing: "CI failing"
        case .watched: "Watched for review"
        case .verdicts: "Has a verdict"
        }
    }

    func matches(_ item: GitHubItem, review: PRReview?) -> Bool {
        switch self {
        case .open: true
        case .needsAction: item.attention != nil
        case .draft: item.isDraft
        case .ciFailing: item.ciState == "failure"
        case .watched: review?.state?.isActive == true
        case .verdicts: review?.verdict != nil
        }
    }
}

/// The PRs to list: only PRs, narrowed by role, state, repo (nil for all) and
/// a title or number search; PRs that need the user first, then newest.
public func filterPRs(_ items: [GitHubItem], reviews: [String: PRReview], role: PRRoleFilter, state: PRStateFilter,
                      repo: String?, search: String) -> [GitHubItem] {
    let query = search.trimmingCharacters(in: .whitespaces)
    return items.filter { item in
        guard item.isPR, role.matches(item), state.matches(item, review: reviews[item.url]) else { return false }
        if let repo, item.repo.caseInsensitiveCompare(repo) != .orderedSame { return false }
        if query.isEmpty { return true }
        return item.title.localizedCaseInsensitiveContains(query) || "#\(item.number)" == query
            || "\(item.number)" == query
    }
    .sorted { a, b in
        if (a.attention != nil) != (b.attention != nil) { return a.attention != nil }
        return (a.updatedAt ?? .distantPast) > (b.updatedAt ?? .distantPast)
    }
}

/// The repos with open PRs, for the repo filter, sorted case-insensitively.
public func prRepos(_ items: [GitHubItem]) -> [String] {
    var seen = Set<String>()
    var out: [String] = []
    for item in items where item.isPR && seen.insert(item.repo.lowercased()).inserted {
        out.append(item.repo)
    }
    return out.sorted { $0.lowercased() < $1.lowercased() }
}

/// `displaySafe` for multi-line text: each line escaped on its own, so line
/// breaks stay line breaks while every other control or format character is
/// shown as an escape.
public func displaySafeLines(_ s: String) -> String {
    // "\r\n" is one Character in Swift, so it is normalized before splitting.
    s.replacingOccurrences(of: "\r\n", with: "\n")
        .split(separator: "\n", omittingEmptySubsequences: false)
        .map { displaySafe(String($0)) }
        .joined(separator: "\n")
}
