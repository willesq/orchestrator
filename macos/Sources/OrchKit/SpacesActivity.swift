import Foundation

/// One record of a session's command log (ListToolEvents), as stored:
/// text is credential-redacted and capped by orchd.
public struct ToolLogEvent: Sendable, Equatable {
    public var seq: Int64
    public var at: Date
    /// event | gap | end
    public var kind: String
    /// pre-tool-use | cwd-changed
    public var event: String
    public var tool: String
    public var agentType: String
    public var cwd: String
    public var oldCwd: String
    public var newCwd: String
    public var command: String
    /// The call's targets as JSON.
    public var targets: String
    /// logged | refused | not enforced
    public var decision: String
    /// The guard's rule, or "would refuse (<rule>)" for a call it did not
    /// enforce.
    public var rule: String
    public var dropped: Int64

    public init(seq: Int64, at: Date, kind: String = "event", event: String = "pre-tool-use", tool: String = "",
                agentType: String = "", cwd: String = "", oldCwd: String = "", newCwd: String = "", command: String = "",
                targets: String = "", decision: String = "", rule: String = "", dropped: Int64 = 0) {
        self.seq = seq
        self.at = at
        self.kind = kind
        self.event = event
        self.tool = tool
        self.agentType = agentType
        self.cwd = cwd
        self.oldCwd = oldCwd
        self.newCwd = newCwd
        self.command = command
        self.targets = targets
        self.decision = decision
        self.rule = rule
        self.dropped = dropped
    }
}

/// One page of a command log.
public struct ToolLogPage: Sendable, Equatable {
    public var events: [ToolLogEvent]
    /// Records after the last one returned remain.
    public var more: Bool

    public init(events: [ToolLogEvent], more: Bool = false) {
        self.events = events
        self.more = more
    }
}

/// What the Activity page asks orchd. The operator reads any session's
/// log; a session only its own (INV-12).
public protocol SpacesActivityAPI: Sendable {
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession]
    func toolEvents(owner: String, afterSeq: Int64, limit: Int32) async throws -> ToolLogPage
}

/// One row of the Activity table.
public struct ActivityRow: Sendable, Equatable, Identifiable {
    public enum Kind: String, Sendable {
        case call, move, gap, end
    }

    /// How the guard decided, for the row's color.
    public enum Verdict: String, Sendable {
        case refused, wouldRefuse, notEnforced, logged, none
    }

    public var id: Int64
    public var at: Date
    public var kind: Kind
    /// The tool, "Directory move", "Events lost" or "Session ended".
    public var title: String
    /// Where the call ran, with ~ for the home folder.
    public var directory: String
    /// The command, the targets, or the move's two folders.
    public var detail: String
    /// orchd's decision with its rule, e.g. "refused · other_isolated_root".
    public var decision: String
    public var verdict: Verdict
    /// A subagent's type, when a subagent made the call.
    public var agent: String

    public var isRefusal: Bool { verdict == .refused || verdict == .wouldRefuse }

    public static func build(_ e: ToolLogEvent, home: String) -> ActivityRow {
        let dir = SpacesOverview.abbreviate(e.cwd, home: home)
        switch e.kind {
        case "gap":
            return ActivityRow(id: e.seq, at: e.at, kind: .gap, title: "Events lost", directory: "",
                               detail: e.dropped == 1 ? "1 event was dropped before it was stored"
                                   : "\(e.dropped) events were dropped before they were stored",
                               decision: "", verdict: .none, agent: "")
        case "end":
            return ActivityRow(id: e.seq, at: e.at, kind: .end, title: "Session ended", directory: "", detail: "",
                               decision: "", verdict: .none, agent: "")
        default:
            break
        }
        let verdict: Verdict
        switch e.decision {
        case "refused": verdict = .refused
        case _ where e.rule.hasPrefix("would refuse"): verdict = .wouldRefuse
        case "not enforced": verdict = .notEnforced
        case "logged": verdict = .logged
        default: verdict = .none
        }
        var decision = displaySafe(e.decision)
        if !e.rule.isEmpty { decision += (decision.isEmpty ? "" : " · ") + displaySafe(e.rule) }
        if e.event == "cwd-changed" {
            let from = SpacesOverview.abbreviate(e.oldCwd, home: home), to = SpacesOverview.abbreviate(e.newCwd, home: home)
            return ActivityRow(id: e.seq, at: e.at, kind: .move, title: "Directory move", directory: dir,
                               detail: "\(from) → \(to)", decision: decision, verdict: verdict, agent: displaySafe(e.agentType))
        }
        let detail = e.command.isEmpty ? e.targets : e.command
        return ActivityRow(id: e.seq, at: e.at, kind: .call, title: e.tool.isEmpty ? "Tool call" : displaySafe(e.tool),
                           directory: dir, detail: displaySafe(detail), decision: decision, verdict: verdict,
                           agent: displaySafe(e.agentType))
    }
}

/// Which rows the Activity table shows.
public enum ActivityFilter: String, Sendable, CaseIterable, Identifiable {
    case all, moves, refusals

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .all: "All"
        case .moves: "Directory Moves"
        case .refusals: "Refusals"
        }
    }

    public func admits(_ row: ActivityRow) -> Bool {
        switch self {
        case .all: true
        case .moves: row.kind == .move
        case .refusals: row.isRefusal
        }
    }
}

/// A pinned session the Activity page can show the log of.
public struct ActivitySession: Sendable, Equatable, Identifiable {
    public var id: String
    /// "Claude Code · 4f2a1b2c"
    public var title: String
    public var space: String
    public var directory: String
    public var state: String
    public var toolEvents: Int64
    public var refusals: Int64
    public var lastActivity: Date?
}
