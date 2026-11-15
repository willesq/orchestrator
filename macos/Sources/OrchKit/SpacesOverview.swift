import Foundation

/// One space of the applied policy, as ListSpaces reports it.
public struct SpaceInfo: Sendable, Equatable {
    public var name: String
    /// public | private | isolated
    public var kind: String
    public var githubOrgs: [String]
    public var roots: [String]
    public var webPrefixes: [String]
    public var chat: [String]
    /// The spaces it lists; `readable` adds itself and every public space.
    public var reads: [String]
    public var readable: [String]
    /// The outbound term list's path, never its terms.
    public var termsFile: String
    public var ghConfigDir: String

    public init(name: String, kind: String, githubOrgs: [String] = [], roots: [String] = [], webPrefixes: [String] = [],
                chat: [String] = [], reads: [String] = [], readable: [String] = [], termsFile: String = "",
                ghConfigDir: String = "") {
        self.name = name
        self.kind = kind
        self.githubOrgs = githubOrgs
        self.roots = roots
        self.webPrefixes = webPrefixes
        self.chat = chat
        self.reads = reads
        self.readable = readable
        self.termsFile = termsFile
        self.ghConfigDir = ghConfigDir
    }
}

/// The policy orchd enforces and any drift from it. It names no space.
public struct SpacesPolicyStatus: Sendable, Equatable {
    public var configured: Bool
    public var appliedRevision: Int64
    public var draftRevision: Int64
    /// "draft" and/or "changed_outside_orchd".
    public var drift: [String]
    public var error: String

    public init(configured: Bool = false, appliedRevision: Int64 = 0, draftRevision: Int64 = 0, drift: [String] = [],
                error: String = "") {
        self.configured = configured
        self.appliedRevision = appliedRevision
        self.draftRevision = draftRevision
        self.drift = drift
        self.error = error
    }
}

/// ListSpaces' answer.
public struct SpacesListing: Sendable, Equatable {
    public var status: SpacesPolicyStatus
    public var spaces: [SpaceInfo]

    public init(status: SpacesPolicyStatus = SpacesPolicyStatus(), spaces: [SpaceInfo] = []) {
        self.status = status
        self.spaces = spaces
    }
}

/// A session pin with its liveness, or a harness process with no pin
/// (ListSessions).
public struct PinnedSession: Sendable, Equatable {
    public var harness: String
    /// "" when unpinned.
    public var sessionID: String
    public var harnessPID: Int64
    /// "" with no policy applied or unpinned; "quarantine" when its folder
    /// could not be placed.
    public var space: String
    public var firstCwd: String
    /// active | idle | ended | unpinned
    public var state: String
    public var lastActivity: Date?
    public var toolEvents: Int64
    public var refusals: Int64

    public init(harness: String, sessionID: String = "", harnessPID: Int64 = 0, space: String = "", firstCwd: String = "",
                state: String, lastActivity: Date? = nil, toolEvents: Int64 = 0, refusals: Int64 = 0) {
        self.harness = harness
        self.sessionID = sessionID
        self.harnessPID = harnessPID
        self.space = space
        self.firstCwd = firstCwd
        self.state = state
        self.lastActivity = lastActivity
        self.toolEvents = toolEvents
        self.refusals = refusals
    }

    /// Whether its process still runs with a pin.
    public var isLive: Bool { state == "active" || state == "idle" }
}

/// One space's line of the command log summary (SummarizeToolLog).
public struct SpaceActivity: Sendable, Equatable {
    public var space: String
    public var sessions: Int
    public var toolCalls: Int64
    public var directoryMoves: Int64
    /// Moves into a folder a session would be pinned to another space for.
    public var crossSpaceMoves: Int64
    public var refusals: Int64

    public init(space: String, sessions: Int = 0, toolCalls: Int64 = 0, directoryMoves: Int64 = 0, crossSpaceMoves: Int64 = 0,
                refusals: Int64 = 0) {
        self.space = space
        self.sessions = sessions
        self.toolCalls = toolCalls
        self.directoryMoves = directoryMoves
        self.crossSpaceMoves = crossSpaceMoves
        self.refusals = refusals
    }
}

/// How far a relabel of stored content has got (SP-027).
public struct RelabelProgress: Sendable, Equatable {
    public var done: Int64
    public var total: Int64

    public init(done: Int64, total: Int64) {
        self.done = done
        self.total = total
    }

    public var isRunning: Bool { total > 0 && done < total }
    public var percent: Int { total > 0 ? Int(min(done, total) * 100 / total) : 0 }

    /// "40%", or the chunks to relabel while none is known done: Relabel
    /// answers only when it ends, so a run from the pane (SP-079) has no
    /// percentage to show.
    public var summary: String {
        if done > 0 { return "\(percent)%" }
        return total == 1 ? "1 chunk" : "\(total.formatted()) chunks"
    }
}

/// The reads the Spaces pane's Overview needs, beyond the switches and the
/// validator. Every call is operator-only in orchd.
public protocol SpacesOverviewAPI: SpacesAPI {
    func listSpaces() async throws -> SpacesListing
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession]
    /// The command log per space over the last 7 days.
    func toolLogSummary() async throws -> [SpaceActivity]
    /// Applied GitHub orgs whose new content goes to quarantine until the
    /// operator confirms them (DiffSpaces).
    func unconfirmedGitHubOrgs() async throws -> [String]
}

/// What the sidebar's Spaces row shows, the most pressing first:
/// conflicts, then quarantine, then drift, then relabel progress.
public enum SpacesBadge: Sendable, Equatable {
    case none
    case notSetUp
    case conflicts(Int)
    case quarantine(Int)
    case drift([String])
    case relabel(RelabelProgress)

    /// Picks the badge. Each kind shows only when nothing before it in the
    /// order needs the user.
    public static func pick(configured: Bool, conflicts: Int, quarantine: Int, drift: [String],
                            relabel: RelabelProgress?) -> SpacesBadge {
        if conflicts > 0 { return .conflicts(conflicts) }
        if quarantine > 0 { return .quarantine(quarantine) }
        if !drift.isEmpty { return .drift(drift) }
        if let relabel, relabel.isRunning { return .relabel(relabel) }
        return configured ? .none : .notSetUp
    }

    /// What the badge says, for its tooltip and VoiceOver.
    public var label: String {
        switch self {
        case .none: ""
        case .notSetUp: "Not set up"
        case .conflicts(let n): n == 1 ? "1 conflict" : "\(n) conflicts"
        case .quarantine(let n): n == 1 ? "1 item in quarantine" : "\(n) items in quarantine"
        case .drift(let kinds): kinds.map(SpacesOverview.driftText).joined(separator: "; ")
        case .relabel(let p): "Relabelling: \(p.summary)"
        }
    }
}

/// Basic hides detail, never something that needs the user.
public enum SpacesDetail: String, Sendable, CaseIterable, Identifiable {
    case basic, advanced

    public var id: String { rawValue }
    public var title: String { self == .basic ? "Basic" : "Advanced" }
}

/// A line of a space card: what kind of binding, and its values.
public struct SpaceBinding: Sendable, Equatable, Identifiable {
    public var id: String { label }
    public var label: String
    public var values: [String]
}

/// One space at a glance.
public struct SpaceCard: Sendable, Equatable, Identifiable {
    /// The space's name as orchd has it, for choosing and filtering; `name`
    /// is the same made safe to show.
    public var id: String
    public var name: String
    public var kind: String
    /// "Isolated · job", "Private · personal", "Public · readable everywhere".
    public var kindLabel: String
    public var bindings: [SpaceBinding]
    /// The other spaces it reads; Advanced only, and never in
    /// screen-share-safe mode.
    public var reads: [String]
    public var liveSessions: Int
    public var toolCalls: Int64
    public var directoryMoves: Int64
    public var crossSpaceMoves: Int64
    public var refusals: Int64
}

/// One session row.
public struct SessionLine: Sendable, Equatable, Identifiable {
    public var id: String
    /// "Claude Code · 4f2a1b2c"
    public var title: String
    public var state: String
    /// The space as orchd has it, for filtering; `space` is made safe to show.
    public var spaceKey: String
    public var space: String
    /// The pinned folder, with ~ for the home folder.
    public var directory: String
    public var toolEvents: Int64
    public var refusals: Int64
    public var lastActivity: Date?
    /// The harness process; shown in Advanced.
    public var pid: Int64
}

/// Something in the needs-you strip.
public struct NeedsYouItem: Sendable, Equatable, Identifiable {
    public enum Kind: String, Sendable {
        case conflict, quarantine, drift, relabel, refusals
    }

    public var id: String { kind.rawValue }
    public var kind: Kind
    public var title: String
    /// One line per folder, org or session it is about.
    public var details: [String]
}

/// Everything the Overview shows, built from orchd's answers. In
/// screen-share-safe mode it holds one space's data and nothing of any other.
public struct SpacesOverview: Sendable, Equatable {
    public var configured: Bool
    public var appliedRevision: Int64
    public var draftRevision: Int64
    /// The policy's drift kinds; they name no space.
    public var drift: [String]
    public var cards: [SpaceCard]
    public var sessions: [SessionLine]
    public var needsYou: [NeedsYouItem]
    public var badge: SpacesBadge
    /// The space screen-share-safe mode shows; nil when every space shows.
    public var shareSafe: String?
    /// Reads that failed while the rest loaded.
    public var problems: [String]

    /// Most sessions in quarantine whose folder is asked about to tell a
    /// conflict from other quarantine, per refresh.
    public static let conflictChecks = 20

    /// The drift kind for people.
    public static func driftText(_ kind: String) -> String {
        switch kind {
        case "draft": "A saved draft is not applied"
        case "changed_outside_orchd": "The applied policy was changed outside orchd"
        case "error": "The applied policy cannot be read"
        default: displaySafe(kind)
        }
    }

    /// "Isolated · job" and the like.
    public static func kindLabel(_ kind: String) -> String {
        switch kind {
        case "public": "Public · readable everywhere"
        case "private": "Private · personal"
        case "isolated": "Isolated · job"
        default: displaySafe(kind)
        }
    }

    /// "Claude Code", "Codex".
    public static func harnessName(_ harness: String) -> String {
        switch harness {
        case "claude": "Claude Code"
        case "codex": "Codex"
        case "gemini": "Gemini"
        default: displaySafe(harness)
        }
    }

    /// `path` with the home folder as ~.
    public static func abbreviate(_ path: String, home: String) -> String {
        guard !home.isEmpty, home != "/" else { return displaySafe(path) }
        if path == home { return "~" }
        if path.hasPrefix(home + "/") { return displaySafe("~" + path.dropFirst(home.count)) }
        return displaySafe(path)
    }

    /// Builds the Overview of every space. `conflicted` names the quarantined
    /// sessions whose folder orchd places in a conflict.
    public static func build(listing: SpacesListing, sessions: [PinnedSession], activity: [SpaceActivity],
                             conflicted: Set<String>, unconfirmedOrgs: [String], relabel: RelabelProgress?,
                             detail: SpacesDetail, home: String, problems: [String] = []) -> SpacesOverview {
        let status = listing.status
        let byActivity = Dictionary(activity.map { ($0.space, $0) }, uniquingKeysWith: { a, _ in a })
        let cards = listing.spaces.map { space in
            card(space, activity: byActivity[space.name], live: sessions.filter { $0.isLive && $0.space == space.name }.count,
                 detail: detail, home: home)
        }
        let shown = sessions.filter { detail == .advanced || $0.state != "ended" }
        let lines = shown.map { line($0, home: home) }

        var needs: [NeedsYouItem] = []
        let live = sessions.filter(\.isLive)
        let conflicts = live.filter { $0.space == "quarantine" && conflicted.contains($0.sessionID) }
        let quarantined = live.filter { $0.space == "quarantine" && !conflicted.contains($0.sessionID) }
        if !conflicts.isEmpty {
            needs.append(NeedsYouItem(
                kind: .conflict,
                title: conflicts.count == 1 ? "1 session in a conflict: nothing there is indexed"
                    : "\(conflicts.count) sessions in a conflict: nothing there is indexed",
                details: conflicts.map { abbreviate($0.firstCwd, home: home) }
            ))
        }
        let quarantineCount = quarantined.count + unconfirmedOrgs.count
        if quarantineCount > 0 {
            var details = quarantined.map { abbreviate($0.firstCwd, home: home) + " (session reads nothing)" }
            details += unconfirmedOrgs.map { "GitHub org \(displaySafe($0)): new content goes to quarantine until you confirm it" }
            needs.append(NeedsYouItem(
                kind: .quarantine,
                title: quarantineCount == 1 ? "1 item in quarantine" : "\(quarantineCount) items in quarantine",
                details: details
            ))
        }
        var drift = status.drift
        if !status.error.isEmpty { drift.append("error") }
        if !drift.isEmpty {
            needs.append(NeedsYouItem(kind: .drift, title: "The policy has drifted", details: drift.map(driftText)))
        }
        if let relabel, relabel.isRunning {
            needs.append(NeedsYouItem(kind: .relabel, title: "Relabelling stored content: \(relabel.summary)", details: []))
        }
        let refusals = activity.reduce(Int64(0)) { $0 + $1.refusals }
        if refusals > 0 {
            needs.append(NeedsYouItem(kind: .refusals,
                                      title: refusals == 1 ? "1 refused tool call in the last 7 days"
                                          : "\(refusals) refused tool calls in the last 7 days",
                                      details: []))
        }
        return SpacesOverview(
            configured: status.configured, appliedRevision: status.appliedRevision, draftRevision: status.draftRevision,
            drift: drift, cards: cards, sessions: lines, needsYou: needs,
            badge: .pick(configured: status.configured, conflicts: conflicts.count, quarantine: quarantineCount,
                         drift: drift, relabel: relabel),
            shareSafe: nil, problems: problems.map(displaySafe)
        )
    }

    /// The Overview for screen-share-safe mode: `space`'s card without the
    /// other spaces it reads, its own sessions, and only what names no other
    /// space. Conflicts, quarantine and relabel progress are about other
    /// spaces' content, so they wait until every space shows again.
    public func restricted(to space: String) -> SpacesOverview {
        var cards = cards.filter { $0.id == space }
        for i in cards.indices { cards[i].reads = [] }
        let needs = needsYou.filter { $0.kind == .drift }
        var restricted = SpacesOverview(
            configured: configured, appliedRevision: appliedRevision, draftRevision: draftRevision,
            drift: drift, cards: cards, sessions: sessions.filter { $0.spaceKey == space }, needsYou: needs,
            badge: .pick(configured: configured, conflicts: 0, quarantine: 0, drift: drift, relabel: nil),
            shareSafe: space, problems: []
        )
        if let mine = cards.first, mine.refusals > 0 {
            restricted.needsYou.append(NeedsYouItem(
                kind: .refusals,
                title: mine.refusals == 1 ? "1 refused tool call in the last 7 days"
                    : "\(mine.refusals) refused tool calls in the last 7 days",
                details: []
            ))
        }
        return restricted
    }

    private static func card(_ s: SpaceInfo, activity: SpaceActivity?, live: Int, detail: SpacesDetail,
                             home: String) -> SpaceCard {
        var bindings: [SpaceBinding] = []
        func add(_ label: String, _ values: [String]) {
            if !values.isEmpty { bindings.append(SpaceBinding(label: label, values: values)) }
        }
        add("GitHub", s.githubOrgs.map(displaySafe))
        add("Folders", s.roots.map { abbreviate($0, home: home) })
        if detail == .advanced {
            add("Web", s.webPrefixes.map(displaySafe))
            add("Chat", s.chat.map(displaySafe))
            if !s.ghConfigDir.isEmpty { add("gh profile", [abbreviate(s.ghConfigDir, home: home)]) }
            if !s.termsFile.isEmpty { add("Outbound terms", [abbreviate(s.termsFile, home: home)]) }
        }
        return SpaceCard(
            id: s.name, name: displaySafe(s.name), kind: s.kind, kindLabel: kindLabel(s.kind), bindings: bindings,
            reads: detail == .advanced ? s.reads.filter { $0 != s.name }.map(displaySafe) : [],
            liveSessions: live, toolCalls: activity?.toolCalls ?? 0, directoryMoves: activity?.directoryMoves ?? 0,
            crossSpaceMoves: activity?.crossSpaceMoves ?? 0, refusals: activity?.refusals ?? 0
        )
    }

    private static func line(_ s: PinnedSession, home: String) -> SessionLine {
        let short = s.sessionID.isEmpty ? "PID \(s.harnessPID)" : String(s.sessionID.prefix(8))
        return SessionLine(
            id: s.sessionID.isEmpty ? "pid:\(s.harnessPID)" : s.sessionID,
            title: harnessName(s.harness) + " · " + displaySafe(short),
            state: displaySafe(s.state), spaceKey: s.space, space: displaySafe(s.space), directory: abbreviate(s.firstCwd, home: home),
            toolEvents: s.toolEvents, refusals: s.refusals, lastActivity: s.lastActivity, pid: s.harnessPID
        )
    }
}
