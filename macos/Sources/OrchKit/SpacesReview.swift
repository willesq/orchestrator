import Foundation

/// One check a spaces draft failed, as orchd reports it.
public struct SpacesProblemInfo: Sendable, Equatable, Hashable {
    /// The rule id, e.g. "isolated_reads_isolated".
    public var rule: String
    public var space: String
    public var message: String

    public init(rule: String, space: String = "", message: String) {
        self.rule = rule
        self.space = space
        self.message = message
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

/// DiffSpaces' answer: the enforced revision, the newest draft and its
/// problems.
public struct SpacesPolicyDiff: Sendable, Equatable {
    /// 0 when nothing is applied.
    public var appliedRevision: Int64
    public var appliedJSON: String
    /// 0 when no draft is newer than the applied revision.
    public var draftRevision: Int64
    public var draftJSON: String
    /// How each space differs, as orchd summarizes it (the setup wizard's review).
    public var changes: [SpacesPolicyChange]
    public var problems: [SpacesProblemInfo]
    public var draftCreatedBy: String
    public var draftCreatedAt: Date?
    /// False when an agent session saved the draft.
    public var draftByOperator: Bool
    /// Applied orgs whose GitHub identity is not confirmed.
    public var unconfirmedGitHubOrgs: [String]

    public init(appliedRevision: Int64 = 0, appliedJSON: String = "", draftRevision: Int64 = 0, draftJSON: String = "",
                changes: [SpacesPolicyChange] = [], problems: [SpacesProblemInfo] = [], draftCreatedBy: String = "", draftCreatedAt: Date? = nil,
                draftByOperator: Bool = true, unconfirmedGitHubOrgs: [String] = []) {
        self.appliedRevision = appliedRevision
        self.appliedJSON = appliedJSON
        self.draftRevision = draftRevision
        self.draftJSON = draftJSON
        self.changes = changes
        self.problems = problems
        self.draftCreatedBy = draftCreatedBy
        self.draftCreatedAt = draftCreatedAt
        self.draftByOperator = draftByOperator
        self.unconfirmedGitHubOrgs = unconfirmedGitHubOrgs
    }
}

/// A JSON document kept whole, so a draft the app edits keeps every field,
/// including ones this app version does not know.
public indirect enum JSONValue: Sendable, Equatable, Codable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: any Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self = .null
        } else if let b = try? c.decode(Bool.self) {
            self = .bool(b)
        } else if let n = try? c.decode(Double.self) {
            self = .number(n)
        } else if let s = try? c.decode(String.self) {
            self = .string(s)
        } else if let a = try? c.decode([JSONValue].self) {
            self = .array(a)
        } else {
            self = .object(try c.decode([String: JSONValue].self))
        }
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let b): try c.encode(b)
        case .number(let n): try c.encode(n)
        case .string(let s): try c.encode(s)
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }

    public static func parse(_ text: String) -> JSONValue? {
        try? JSONDecoder().decode(JSONValue.self, from: Data(text.utf8))
    }

    /// Compact JSON with sorted keys.
    public var text: String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return (try? encoder.encode(self)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }

    public subscript(key: String) -> JSONValue? {
        if case .object(let o) = self { return o[key] }
        return nil
    }

    public var arrayValue: [JSONValue]? {
        if case .array(let a) = self { return a }
        return nil
    }

    public var stringValue: String? {
        if case .string(let s) = self { return s }
        return nil
    }

    /// A value as one line shows it: strings bare, anything else as JSON.
    public var display: String { stringValue ?? text }
}

/// One line of the review: a space, or one binding or setting of it, and
/// how it differs from the applied revision.
public struct ReviewLine: Sendable, Equatable, Identifiable {
    public enum Change: String, Sendable {
        case same, added, removed
    }

    public var id: String
    public var change: Change
    /// The space it belongs to, made safe to show; "" for the policy's own
    /// fields.
    public var space: String
    /// "" for a space's own line; otherwise the field, e.g. "reads".
    public var field: String
    /// What the line shows, made safe to show.
    public var text: String
    /// Indented under its space.
    public var nested: Bool
    /// The draft's problems this line causes.
    public var problems: [SpacesProblemInfo]
    /// Dropping the line reverts it to the applied revision. False for a
    /// line the applied revision already has: removing it could move
    /// content somewhere less strict, so it is fixed in the draft instead.
    public var excludable: Bool

    public var invalid: Bool { !problems.isEmpty }
}

/// What applying the reviewed revision changes, as counts.
public struct ReviewEffect: Sendable, Equatable, Identifiable {
    public var id: String { label }
    public var label: String
    public var count: Int
}

/// Everything the Review page shows for one draft revision.
public struct SpacesReview: Sendable, Equatable {
    public var appliedRevision: Int64
    /// The revision Apply names: what is shown, never a newer one.
    public var reviewedRevision: Int64
    public var lines: [ReviewLine]
    /// Problems no line could be found for; they block Apply.
    public var unplaced: [SpacesProblemInfo]
    public var effects: [ReviewEffect]
    /// The draft could not be read as a policy; its text is shown raw.
    public var unreadable: Bool

    public var invalidLines: [ReviewLine] { lines.filter(\.invalid) }
    /// Lines the app can drop for the user, saving a new draft.
    public var excludableCount: Int { lines.filter { $0.invalid && $0.excludable }.count }
    /// orchd refuses to apply a draft with any problem.
    public var hasProblems: Bool { !invalidLines.isEmpty || !unplaced.isEmpty }

    /// Builds the review of `diff`'s draft against its applied revision.
    /// `home` expands ~ in roots the way orchd reports them.
    public static func build(_ diff: SpacesPolicyDiff, home: String) -> SpacesReview {
        guard diff.draftRevision > 0 else {
            return SpacesReview(appliedRevision: diff.appliedRevision, reviewedRevision: 0, lines: [], unplaced: [],
                                effects: [], unreadable: false)
        }
        let applied = JSONValue.parse(diff.appliedJSON) ?? .object([:])
        guard let draft = JSONValue.parse(diff.draftJSON), case .object = draft else {
            let lines = diff.draftJSON.split(separator: "\n", omittingEmptySubsequences: false).enumerated().map {
                ReviewLine(id: "raw/\($0.offset)", change: .added, space: "", field: "", text: displaySafe(String($0.element)),
                           nested: false, problems: [], excludable: false)
            }
            return SpacesReview(appliedRevision: diff.appliedRevision, reviewedRevision: diff.draftRevision, lines: lines,
                                unplaced: diff.problems, effects: [], unreadable: true)
        }
        let doc = PolicyDoc(draft: draft, applied: applied, home: home)
        let placed = doc.place(diff.problems)
        let lines = doc.lines(problems: placed.byTarget).map { line in
            var l = line
            l.excludable = l.invalid && l.excludable
            return l
        }
        return SpacesReview(appliedRevision: diff.appliedRevision, reviewedRevision: diff.draftRevision, lines: lines,
                            unplaced: placed.unplaced, effects: PolicyDoc.effects(lines), unreadable: false)
    }

    /// The draft with every excludable invalid line reverted to the applied
    /// revision, as a policy document to save as a new draft; nil when
    /// there is nothing to drop.
    public static func excluding(_ diff: SpacesPolicyDiff, home: String) -> String? {
        guard diff.draftRevision > 0, let draft = JSONValue.parse(diff.draftJSON), case .object = draft else { return nil }
        let doc = PolicyDoc(draft: draft, applied: JSONValue.parse(diff.appliedJSON) ?? .object([:]), home: home)
        let placed = doc.place(diff.problems).byTarget
        let targets = placed.keys.filter { doc.droppable($0, placed[$0] ?? []) }
        guard !targets.isEmpty else { return nil }
        return doc.reverting(Set(targets)).text
    }
}
