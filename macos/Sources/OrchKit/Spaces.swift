import Foundation

/// One enforcement layer's switch, as orchd reports it (SP-070).
public struct EnforcementSwitch: Sendable, Equatable, Identifiable {
    public var id: String { layer }
    public var layer: String
    public var title: String
    public var state: String
    public var defaultState: String
    public var states: [String]
    /// The layer is not enforcing: switched off, the guard only logging, or
    /// a `problem` outside the switch.
    public var relaxed: Bool
    public var setAt: Date?
    /// Who switched it; orchd tells the operator only.
    public var setBy: String
    /// When a timed switch reverts by itself; nil when untimed.
    public var until: Date?
    public var revertsTo: String
    /// Why the layer does not enforce though its switch is on.
    public var problem: String

    public init(layer: String, title: String = "", state: String, defaultState: String = "", states: [String] = [],
                relaxed: Bool = false, setAt: Date? = nil, setBy: String = "", until: Date? = nil,
                revertsTo: String = "", problem: String = "") {
        self.layer = layer
        self.title = title
        self.state = state
        self.defaultState = defaultState
        self.states = states
        self.relaxed = relaxed
        self.setAt = setAt
        self.setBy = setBy
        self.until = until
        self.revertsTo = revertsTo
        self.problem = problem
    }

    /// The layer for people: orchd's title, else its name.
    public var name: String { title.isEmpty ? layer : title }

    /// Whether choosing `state` would stop the layer enforcing. Mirrors
    /// orchd's rule: off for every layer, and log for the guard.
    public func relaxes(to state: String) -> Bool {
        state == "off" || (layer == "guard" && state == "log")
    }

    /// Seconds left on a timed switch at `now`, never negative; nil when
    /// the switch is untimed.
    public func remaining(at now: Date) -> TimeInterval? {
        guard let until else { return nil }
        return max(0, until.timeIntervalSince(now))
    }

    /// The timer, as "reverts to on in 14:59", at `now`.
    public func countdown(at now: Date) -> String? {
        guard let left = remaining(at: now) else { return nil }
        let target = revertsTo.isEmpty ? "its earlier state" : displaySafe(revertsTo)
        return "reverts to \(target) in \(Self.clock(left))"
    }

    /// 14:59 under an hour, 1:02:03 from an hour up.
    public static func clock(_ seconds: TimeInterval) -> String {
        let s = Int(seconds.rounded(.up))
        let (h, m, sec) = (s / 3600, s % 3600 / 60, s % 60)
        return h > 0 ? String(format: "%d:%02d:%02d", h, m, sec) : String(format: "%d:%02d", m, sec)
    }
}

/// Every layer's switch, and what orchd could not read.
public struct EnforcementState: Sendable, Equatable {
    public var switches: [EnforcementSwitch]
    /// Set when orchd could not read the switches, or orch.db's differ from
    /// the ones it enforces; the affected layers enforce.
    public var error: String

    public init(switches: [EnforcementSwitch] = [], error: String = "") {
        self.switches = switches
        self.error = error
    }

    /// The layers that are not enforcing now.
    public var relaxed: [EnforcementSwitch] { switches.filter(\.relaxed) }

    /// The earliest moment a timed switch reverts.
    public var nextExpiry: Date? { switches.compactMap(\.until).min() }
}

/// How long a switch holds before it reverts.
public enum EnforcementDuration: Int64, Sendable, CaseIterable, Identifiable {
    case untilChanged = 0
    case minutes15 = 900
    case hour1 = 3600
    case hours4 = 14400
    case hours24 = 86400

    public var id: Int64 { rawValue }
    public var seconds: Int64 { rawValue }

    public var label: String {
        switch self {
        case .untilChanged: "Until I change it"
        case .minutes15: "For 15 minutes"
        case .hour1: "For 1 hour"
        case .hours4: "For 4 hours"
        case .hours24: "For 24 hours"
        }
    }
}

/// What SetEnforcement did.
public struct EnforcementChangeResult: Sendable, Equatable {
    public var current: EnforcementSwitch
    public var changed: Bool
    /// What else the switch did or could not do, for the operator.
    public var notes: [String]

    public init(current: EnforcementSwitch, changed: Bool = true, notes: [String] = []) {
        self.current = current
        self.changed = changed
        self.notes = notes
    }
}

/// A switch the user picked, waiting for them to confirm it.
public struct EnforcementChange: Sendable, Equatable {
    public var layer: EnforcementSwitch
    public var state: String
    public var duration: EnforcementDuration

    public init(layer: EnforcementSwitch, state: String, duration: EnforcementDuration) {
        self.layer = layer
        self.state = state
        self.duration = duration
    }

    // Every string here comes from orchd and goes into the confirmation
    // prompt, so it is made safe to show like the rest of the pane.
    public var title: String {
        let name = displaySafe(layer.name)
        return layer.relaxes(to: state) ? "Stop enforcing \(name)?" : "Switch \(name) to \(displaySafe(state))?"
    }

    public var message: String {
        let when = duration == .untilChanged
            ? "It stays \(displaySafe(state)) until you change it."
            : "It reverts to \(displaySafe(layer.state)) after \(duration.label.dropFirst(4))."
        return "\(displaySafe(layer.name)) becomes \(displaySafe(state)). \(when) The change is logged, and until it is back on every session's start digest says so."
    }
}

/// One bound folder root holding a path.
public struct PlacementRoot: Sendable, Equatable {
    public var root: String
    public var space: String
    public var kind: String

    public init(root: String, space: String, kind: String) {
        self.root = root
        self.space = space
        self.kind = kind
    }
}

/// What a path's GitHub repo contributes to its placement.
public struct PlacementRemote: Sendable, Equatable {
    public var owner: String
    public var repo: String
    /// public | private | "" when not checked.
    public var visibility: String
    /// The owner's space; "" when the owner is bound to none.
    public var space: String
    public var kind: String
    public var effect: String

    public init(owner: String, repo: String, visibility: String = "", space: String = "", kind: String = "", effect: String = "") {
        self.owner = owner
        self.repo = repo
        self.visibility = visibility
        self.space = space
        self.kind = kind
        self.effect = effect
    }

    /// "acme/app (private)".
    public var label: String { visibility.isEmpty ? "\(owner)/\(repo)" : "\(owner)/\(repo) (\(visibility))" }
}

/// Which space content from a target goes to, and why (ExplainPlacement).
public struct PlacementExplanation: Sendable, Equatable {
    public var targetKind: String
    /// Every bound root holding the target, shallowest first; the last decides.
    public var roots: [PlacementRoot]
    public var remote: PlacementRemote?
    public var rule: String
    public var reason: String
    /// placed | quarantined | conflict
    public var outcome: String
    public var space: String
    public var sessionSpace: String
    public var sessionReads: String

    public init(targetKind: String = "path", roots: [PlacementRoot] = [], remote: PlacementRemote? = nil, rule: String = "",
                reason: String = "", outcome: String = "", space: String = "", sessionSpace: String = "", sessionReads: String = "") {
        self.targetKind = targetKind
        self.roots = roots
        self.remote = remote
        self.rule = rule
        self.reason = reason
        self.outcome = outcome
        self.space = space
        self.sessionSpace = sessionSpace
        self.sessionReads = sessionReads
    }

    /// The effective space in a few words.
    public var result: String {
        switch outcome {
        case "placed": space
        case "conflict": "conflict: nothing is indexed"
        default: space.isEmpty ? "quarantine" : space
        }
    }

    /// Whether the git remote changed the space the roots alone would give.
    public var remoteAdjusted: Bool {
        guard remote != nil else { return false }
        return rule == "remote_stricter" || rule == "conflict" || rule == "not_verified_public"
            || (roots.isEmpty && rule == "remote")
    }
}

public protocol SpacesAPI: Sendable {
    func enforcement() async throws -> EnforcementState
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult
    func explainPlacement(target: String) async throws -> PlacementExplanation
}
