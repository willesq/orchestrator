import Foundation

/// One git checkout ScanSpaces found, with where the applied policy places it.
public struct ScannedCheckout: Sendable, Equatable {
    public var path: String
    /// The origin's GitHub owner; "" for none.
    public var owner: String
    public var repo: String
    /// public | private | "" when not known.
    public var visibility: String
    /// Its .git does not follow git's layout, so orchd places it by folder alone.
    public var anonymous: Bool
    /// Where the applied policy places it now.
    public var space: String
    public var rule: String
    public var outcome: String
    public var reason: String

    public init(path: String, owner: String = "", repo: String = "", visibility: String = "", anonymous: Bool = false,
                space: String = "", rule: String = "", outcome: String = "", reason: String = "") {
        self.path = path
        self.owner = owner
        self.repo = repo
        self.visibility = visibility
        self.anonymous = anonymous
        self.space = space
        self.rule = rule
        self.outcome = outcome
        self.reason = reason
    }

    /// Whether orchd will believe its remote: an anonymous checkout is placed
    /// by its folder alone.
    public var hasRemote: Bool { !owner.isEmpty && !anonymous }
}

/// A gh login and the GitHub orgs it belongs to.
public struct ScannedAccount: Sendable, Equatable {
    /// "" for the default login, else the applied space whose profile it is.
    public var profile: String
    public var login: String
    public var orgs: [String]
    public var error: String

    public init(profile: String = "", login: String = "", orgs: [String] = [], error: String = "") {
        self.profile = profile
        self.login = login
        self.orgs = orgs
        self.error = error
    }
}

/// What ScanSpaces found. It writes nothing.
public struct SpacesScan: Sendable, Equatable {
    public var checkouts: [ScannedCheckout]
    public var accounts: [ScannedAccount]
    /// A limit stopped the walk early.
    public var truncated: Bool
    /// Folders that could not be read, and why.
    public var problems: [String]

    public init(checkouts: [ScannedCheckout] = [], accounts: [ScannedAccount] = [], truncated: Bool = false,
                problems: [String] = []) {
        self.checkouts = checkouts
        self.accounts = accounts
        self.truncated = truncated
        self.problems = problems
    }
}

/// A rule a policy breaks, as orchd's validator reports it.
public struct SpacesProblem: Sendable, Equatable {
    public var rule: String
    public var space: String
    public var message: String

    public init(rule: String, space: String = "", message: String) {
        self.rule = rule
        self.space = space
        self.message = message
    }
}

/// A draft revision orchd saved. Saving changes nothing that is enforced.
public struct SavedSpacesDraft: Sendable, Equatable {
    public var revision: Int64
    public var sha256: String
    public var problems: [SpacesProblem]

    public init(revision: Int64, sha256: String = "", problems: [SpacesProblem] = []) {
        self.revision = revision
        self.sha256 = sha256
        self.problems = problems
    }
}

/// How one space differs between the applied revision and the draft.
public struct SpacesPolicyChange: Sendable, Equatable {
    /// "" for the policy's own fields.
    public var space: String
    /// added | removed | changed
    public var kind: String
    public var fields: [String]
    public var details: [String]

    public init(space: String, kind: String, fields: [String] = [], details: [String] = []) {
        self.space = space
        self.kind = kind
        self.fields = fields
        self.details = details
    }
}

/// The applied revision and the newest draft, compared (DiffSpaces).
public struct SpacesPolicyDiff: Sendable, Equatable {
    /// 0: nothing applied.
    public var appliedRevision: Int64
    public var appliedJSON: String
    /// 0: no draft newer than the applied revision.
    public var draftRevision: Int64
    public var changes: [SpacesPolicyChange]
    public var problems: [SpacesProblem]
    public var draftCreatedBy: String
    /// false: an agent session saved the draft.
    public var draftByOperator: Bool

    public init(appliedRevision: Int64 = 0, appliedJSON: String = "", draftRevision: Int64 = 0,
                changes: [SpacesPolicyChange] = [], problems: [SpacesProblem] = [], draftCreatedBy: String = "",
                draftByOperator: Bool = true) {
        self.appliedRevision = appliedRevision
        self.appliedJSON = appliedJSON
        self.draftRevision = draftRevision
        self.changes = changes
        self.problems = problems
        self.draftCreatedBy = draftCreatedBy
        self.draftByOperator = draftByOperator
    }
}

/// Why the setup wizard cannot run against this source.
public struct SpacesSetupUnavailable: LocalizedError, Sendable, Equatable {
    public init() {}
    public var errorDescription: String? { "Setup needs orchd. Start it, then try again." }
}

/// What the setup wizard (SP-052) reads and writes through orchd. Scanning,
/// diffing and applying are operator-only in orchd; under a policy only
/// Orch.app is the operator (D-71), so agents can never apply through this.
public protocol SpacesSetupAPI: SpacesAPI {
    /// Looks through `folders` for git checkouts and lists each gh login's orgs.
    func scanSpaces(folders: [String], offline: Bool) async throws -> SpacesScan
    /// Saves `policyJSON` as a new draft revision; enforces nothing.
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft
    func diffSpaces() async throws -> SpacesPolicyDiff
    /// Enforces the newest draft, which must be `reviewedRevision`.
    func applySpaces(reviewedRevision: Int64) async throws -> Int64
    /// The command log's retention after a session ends: 3d, 7d or 30d.
    func commandLogRetention() async throws -> CommandLogRetention
    func setCommandLogRetention(_ value: String) async throws
}

/// The command log retention setting and the values it takes.
public struct CommandLogRetention: Sendable, Equatable {
    public static let key = "sessions.command_log.retention"
    public var value: String
    public var options: [String]
    /// The change takes effect when orchd restarts.
    public var needsRestart: Bool

    public init(value: String, options: [String] = ["3d", "7d", "30d"], needsRestart: Bool = true) {
        self.value = value
        self.options = options
        self.needsRestart = needsRestart
    }

    /// "7 days after a session ends".
    public static func label(_ value: String) -> String {
        guard value.hasSuffix("d"), let n = Int(value.dropLast()) else { return displaySafe(value) }
        return n == 1 ? "1 day after a session ends" : "\(n) days after a session ends"
    }
}

// Reason: SpacesOverviewAPI inherits this so the Spaces pane's one source
// serves both, and a source with no setup (a test fake, an unreachable
// daemon) still builds; it reports setup as unavailable instead.
extension SpacesSetupAPI {
    public func scanSpaces(folders: [String], offline: Bool) async throws -> SpacesScan { throw SpacesSetupUnavailable() }
    public func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft { throw SpacesSetupUnavailable() }
    public func diffSpaces() async throws -> SpacesPolicyDiff { throw SpacesSetupUnavailable() }
    public func applySpaces(reviewedRevision: Int64) async throws -> Int64 { throw SpacesSetupUnavailable() }
    public func commandLogRetention() async throws -> CommandLogRetention { throw SpacesSetupUnavailable() }
    public func setCommandLogRetention(_ value: String) async throws { throw SpacesSetupUnavailable() }
}
