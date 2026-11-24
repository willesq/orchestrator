import Foundation

/// Where a PR review's project instructions, skills and agents come from.
public enum PRReviewInstructionsSource: String, Sendable, CaseIterable {
    case defaultBranch = "default"
    case base
    case head
    case none

    public var label: String {
        switch self {
        case .defaultBranch: "Default branch"
        case .base: "PR base branch"
        case .head: "PR head"
        case .none: "None"
        }
    }

    public var explanation: String {
        switch self {
        case .defaultBranch: "The repo's instructions as merged, which someone already reviewed."
        case .base: "The branch the PR merges into. For a stacked PR, that branch has not been reviewed yet."
        case .head: "The PR's own instructions, written by its author. Only used when the author is a member or collaborator."
        case .none: "No project instructions, skills or agents: only your own."
        }
    }
}

/// What a PR review's diff is taken against.
public enum PRReviewComparison: String, Sendable, CaseIterable {
    case base
    case defaultBranch = "default"

    public var label: String {
        switch self {
        case .base: "PR base"
        case .defaultBranch: "Default branch"
        }
    }
}

/// Whether a PR review applies the repo's Claude Code settings, which carry its
/// hooks, MCP servers, plugins and permissions.
public enum PRReviewProjectHooks: String, Sendable, CaseIterable {
    case automatic = ""
    case on
    case off

    public var label: String {
        switch self {
        case .automatic: "Automatic"
        case .on: "On"
        case .off: "Off"
        }
    }
}

/// How a PR's reviews are set up: which checkout the reviewing agent starts
/// in and what it compares the PR against.
public struct PRReviewSetup: Sendable, Equatable {
    public var instructionsFrom: PRReviewInstructionsSource
    public var compareAgainst: PRReviewComparison
    public var projectHooks: PRReviewProjectHooks

    public static let standard = PRReviewSetup()

    public init(instructionsFrom: PRReviewInstructionsSource = .defaultBranch, compareAgainst: PRReviewComparison = .base,
                projectHooks: PRReviewProjectHooks = .automatic) {
        self.instructionsFrom = instructionsFrom
        self.compareAgainst = compareAgainst
        self.projectHooks = projectHooks
    }

    /// The setup orchd sent; a value this build does not know reads as the default.
    public init(instructionsFrom: String, compareAgainst: String, projectHooks: String) {
        self.init(instructionsFrom: PRReviewInstructionsSource(rawValue: instructionsFrom) ?? .defaultBranch,
                  compareAgainst: PRReviewComparison(rawValue: compareAgainst) ?? .base,
                  projectHooks: PRReviewProjectHooks(rawValue: projectHooks) ?? .automatic)
    }

    /// Whether moving from old to self lets more of the repo's unreviewed
    /// content steer or run in the review: instructions from a branch that
    /// has not merged, or hooks turned on. Those ask first.
    public func widens(from old: PRReviewSetup) -> Bool {
        let unreviewed: Set<PRReviewInstructionsSource> = [.base, .head]
        if unreviewed.contains(instructionsFrom), instructionsFrom != old.instructionsFrom { return true }
        return projectHooks == .on && old.projectHooks != .on
    }

    /// Why a widening change asks first.
    public var warning: String {
        var parts: [String] = []
        if instructionsFrom == .base || instructionsFrom == .head {
            parts.append("The reviewer will follow instructions and use skills from \(instructionsFrom.label.lowercased()), which nobody has reviewed yet.")
            if projectHooks != .on {
                parts.append("Its settings, hooks and MCP servers stay off unless you turn them on.")
            }
        }
        if projectHooks == .on {
            parts.append("The repo's settings, hooks and MCP servers will run on this Mac with your GitHub sign-in.")
        }
        return parts.joined(separator: " ")
    }
}
