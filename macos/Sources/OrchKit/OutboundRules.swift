import Foundation
import Observation

/// One rule of the outbound terms check (spaces SP-075), as orchd lists it:
/// by id and kind, never by its value, which orchd never returns (INV-07).
public struct OutboundRule: Identifiable, Sendable, Equatable {
    public var id: String
    /// "baseline", or the kind of an added rule.
    public var kind: String
    public var on: Bool
    /// A baseline rule that reads more than the strict reading (D-60).
    public var loosens: Bool
    public var summary: String
    public var baseline: Bool

    public init(id: String, kind: String, on: Bool, loosens: Bool = false, summary: String = "", baseline: Bool = false) {
        self.id = id
        self.kind = kind
        self.on = on
        self.loosens = loosens
        self.summary = summary
        self.baseline = baseline
    }
}

/// One change of the rules, for the operator: no value.
public struct OutboundRuleChangeEntry: Identifiable, Sendable, Equatable {
    public var revision: Int64
    public var at: Date?
    public var by: String
    public var action: String
    public var ruleID: String
    public var id: Int64 { revision }

    public init(revision: Int64, at: Date?, by: String, action: String, ruleID: String) {
        self.revision = revision
        self.at = at
        self.by = by
        self.action = action
        self.ruleID = ruleID
    }
}

/// The outbound rules and the state of the terms layer they belong to.
public struct OutboundRulesState: Sendable, Equatable {
    public var rules: [OutboundRule]
    public var revision: Int64
    public var setAt: Date?
    public var setBy: String
    /// orch.db's rules could not be read; the check then refuses every
    /// shell command and MCP tool while terms apply.
    public var problem: String
    /// The terms layer's switch (SP-070); with it off, no rule applies.
    public var termsLayer: String
    public var history: [OutboundRuleChangeEntry]
    /// The kinds a rule may be added as.
    public var kinds: [String]

    public init(rules: [OutboundRule], revision: Int64 = 0, setAt: Date? = nil, setBy: String = "", problem: String = "",
                termsLayer: String = "on", history: [OutboundRuleChangeEntry] = [], kinds: [String] = ["command", "destination", "fail_closed"]) {
        self.rules = rules
        self.revision = revision
        self.setAt = setAt
        self.setBy = setBy
        self.problem = problem
        self.termsLayer = termsLayer
        self.history = history
        self.kinds = kinds
    }

    /// Whether the whole terms check is switched off, so no rule applies.
    public var termsOff: Bool { !termsLayer.isEmpty && termsLayer != "on" }
    public var baselineRules: [OutboundRule] { rules.filter(\.baseline) }
    public var addedRules: [OutboundRule] { rules.filter { !$0.baseline } }
}

public struct OutboundRuleChangeResult: Sendable, Equatable {
    public var ruleID: String
    public var changed: Bool
    public var revision: Int64
    public var current: OutboundRulesState?

    public init(ruleID: String, changed: Bool, revision: Int64, current: OutboundRulesState? = nil) {
        self.ruleID = ruleID
        self.changed = changed
        self.revision = revision
        self.current = current
    }
}

/// A source that cannot list or change the rules.
public struct OutboundRulesUnavailable: Error, CustomStringConvertible {
    public init() {}
    public var description: String { "orchd cannot list the outbound rules" }
}

extension SpacesAPI {
    /// A source without outbound rules, such as a daemon that cannot be reached.
    public func outboundRules() async throws -> OutboundRulesState { throw OutboundRulesUnavailable() }
    public func changeOutboundRule(action: String, id: String, kind: String, value: String) async throws -> OutboundRuleChangeResult {
        throw OutboundRulesUnavailable()
    }
}

/// A switch of a baseline rule that relaxes the check, waiting for the
/// user to confirm.
public struct OutboundRuleRelaxation: Sendable, Equatable {
    public var rule: OutboundRule
    public var title: String { "Switch off \(displaySafe(rule.id))?" }
    public var message: String {
        "The outbound terms check will read commands less strictly: \(displaySafe(rule.summary)). The change is logged with its time."
    }
}

/// Drives Settings → Spaces → Outbound rules: lists the rules, switches
/// them (asking first before a baseline rule is switched off), adds and
/// removes the operator's own. orchd refuses all of it to anything an agent
/// runs, and the refusal is shown under the rule.
@MainActor
@Observable
public final class OutboundRulesModel {
    public private(set) var state: OutboundRulesState?
    public private(set) var lastError: String?
    /// Rules with a change in flight.
    public private(set) var pending: Set<String> = []
    /// orchd's refusal, per rule id ("" for an add).
    public private(set) var ruleErrors: [String: String] = [:]
    public private(set) var confirmation: OutboundRuleRelaxation?
    /// The id of the rule the last add made.
    public private(set) var lastAdded: String?

    private let api: any SpacesAPI

    public init(api: any SpacesAPI) { self.api = api }

    public func load() async {
        do {
            state = try await api.outboundRules()
            lastError = nil
        } catch {
            lastError = spacesRefusal(error)
        }
    }

    /// The user switched `rule` to `on`. Switching a baseline rule off
    /// relaxes the check, so it waits for `confirm()`.
    public func set(_ rule: OutboundRule, on: Bool) async {
        ruleErrors[rule.id] = nil
        guard rule.on != on else { return }
        if rule.baseline && !on {
            confirmation = OutboundRuleRelaxation(rule: rule)
            return
        }
        await send(id: rule.id, action: on ? "enable" : "disable")
    }

    public func confirm() async {
        guard let c = confirmation else { return }
        confirmation = nil
        await send(id: c.rule.id, action: "disable")
    }

    public func cancel() { confirmation = nil }

    public func remove(_ rule: OutboundRule) async {
        ruleErrors[rule.id] = nil
        await send(id: rule.id, action: "remove")
    }

    /// Adds a stricter rule. The value goes to orchd and is never kept or
    /// shown here. Returns whether orchd took it.
    @discardableResult
    public func add(kind: String, value: String) async -> Bool {
        ruleErrors[""] = nil
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            ruleErrors[""] = "Enter what the rule checks."
            return false
        }
        pending.insert("")
        defer { pending.remove("") }
        do {
            let r = try await api.changeOutboundRule(action: "add", id: "", kind: kind, value: trimmed)
            lastAdded = r.ruleID
            if let current = r.current { state = current } else { await load() }
            return true
        } catch {
            ruleErrors[""] = spacesRefusal(error)
            return false
        }
    }

    private func send(id: String, action: String) async {
        pending.insert(id)
        defer { pending.remove(id) }
        do {
            let r = try await api.changeOutboundRule(action: action, id: id, kind: "", value: "")
            if let current = r.current { state = current } else { await load() }
        } catch {
            ruleErrors[id] = spacesRefusal(error)
        }
    }
}
