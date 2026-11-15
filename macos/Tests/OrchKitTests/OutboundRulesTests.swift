import Foundation
import GRPCCore
import Testing
@testable import OrchKit

/// Behaves like orchd's outbound rules: ids, kinds and states only; an add
/// gets the next user-<n> id; the operator check is a switch.
private final class FakeRules: SpacesAPI, @unchecked Sendable {
    private let lock = NSLock()
    var state = OutboundRulesState(rules: [
        OutboundRule(id: "fail_closed_unreadable", kind: "baseline", on: true, summary: "refuse", baseline: true),
        OutboundRule(id: "plain_variables", kind: "baseline", on: true, loosens: true, summary: "read $PWD", baseline: true),
    ])
    var changes: [(action: String, id: String, kind: String, value: String)] = []
    var refuse: RPCError?
    var next = 0

    func enforcement() async throws -> EnforcementState { EnforcementState(switches: []) }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }
    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }

    func outboundRules() async throws -> OutboundRulesState {
        try lock.withLock {
            if let refuse { throw refuse }
            return state
        }
    }

    func changeOutboundRule(action: String, id: String, kind: String, value: String) async throws -> OutboundRuleChangeResult {
        try lock.withLock {
            changes.append((action, id, kind, value))
            if let refuse { throw refuse }
            var ruleID = id
            switch action {
            case "add":
                next += 1
                ruleID = "user-\(next)"
                state.rules.append(OutboundRule(id: ruleID, kind: kind, on: true, summary: "added"))
            case "remove":
                state.rules.removeAll { $0.id == id }
            default:
                if let i = state.rules.firstIndex(where: { $0.id == id }) { state.rules[i].on = action == "enable" }
            }
            state.revision += 1
            return OutboundRuleChangeResult(ruleID: ruleID, changed: true, revision: state.revision, current: state)
        }
    }
}

@MainActor
@Suite struct OutboundRulesModelTests {
    /// SP-075.2.a2: the app lists baseline and added rules with their
    /// state, and shows whether the terms layer is off.
    @Test func listsBaselineAndAddedRules() async {
        let api = FakeRules()
        api.state.rules.append(OutboundRule(id: "user-1", kind: "command", on: false, summary: "checks a command"))
        api.state.termsLayer = "off"
        let model = OutboundRulesModel(api: api)
        await model.load()
        let state = try! #require(model.state)
        #expect(state.baselineRules.map(\.id) == ["fail_closed_unreadable", "plain_variables"])
        #expect(state.addedRules.map(\.id) == ["user-1"])
        #expect(state.addedRules.first?.on == false)
        #expect(state.termsOff)
        #expect(OutboundRulesState(rules: []).termsOff == false)
    }

    /// Switching a baseline rule off relaxes the check: it waits for the
    /// user to confirm; switching it back on, or an added rule, is sent.
    @Test func confirmsBeforeRelaxing() async {
        let api = FakeRules()
        let model = OutboundRulesModel(api: api)
        await model.load()
        let rule = model.state!.baselineRules[1]
        await model.set(rule, on: false)
        #expect(model.confirmation?.rule.id == "plain_variables")
        #expect(api.changes.isEmpty)
        model.cancel()
        #expect(model.confirmation == nil)
        await model.set(rule, on: false)
        await model.confirm()
        #expect(api.changes.map(\.action) == ["disable"])
        #expect(model.state?.baselineRules[1].on == false)
        await model.set(model.state!.baselineRules[1], on: true)
        #expect(api.changes.map(\.action) == ["disable", "enable"])
        await model.set(model.state!.baselineRules[1], on: true)
        #expect(api.changes.count == 2)
    }

    /// Adding sends the value once and keeps only the new id; removing an
    /// added rule sends its id.
    @Test func addsAndRemoves() async {
        let api = FakeRules()
        let model = OutboundRulesModel(api: api)
        await model.load()
        #expect(await model.add(kind: "fail_closed", value: "   ") == false)
        #expect(model.ruleErrors[""] != nil)
        #expect(await model.add(kind: "fail_closed", value: " deploy-prod "))
        #expect(api.changes.last?.value == "deploy-prod")
        #expect(model.lastAdded == "user-1")
        #expect(model.ruleErrors[""] == nil)
        let added = model.state!.addedRules[0]
        #expect(added.id == "user-1")
        await model.remove(added)
        #expect(api.changes.last?.action == "remove")
        #expect(model.state?.addedRules.isEmpty == true)
    }

    /// orchd's refusal of a non-operator caller is shown under the rule,
    /// or under the add form; a list that fails says why.
    @Test func showsRefusals() async {
        let api = FakeRules()
        let model = OutboundRulesModel(api: api)
        await model.load()
        api.refuse = RPCError(code: .permissionDenied, message: "only the operator can change the outbound rules")
        await model.set(OutboundRule(id: "user-9", kind: "command", on: false), on: true)
        #expect(model.ruleErrors["user-9"] == "only the operator can change the outbound rules")
        #expect(await model.add(kind: "command", value: "glab") == false)
        #expect(model.ruleErrors[""] == "only the operator can change the outbound rules")
        await model.load()
        #expect(model.lastError == "only the operator can change the outbound rules")
    }

    /// A source without outbound rules says so instead of listing nothing.
    @Test func unavailableSource() async {
        struct NoRules: SpacesAPI {
            func enforcement() async throws -> EnforcementState { EnforcementState(switches: []) }
            func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
                throw CancellationError()
            }
            func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
        }
        let model = EnforcementModel(api: NoRules()).outboundRules
        await model.load()
        #expect(model.state == nil)
        #expect(model.lastError != nil)
    }

    /// The wire form maps to the app's, rule for rule.
    @Test func mapsTheWire() {
        var wire = Orchd_V1_OutboundRules()
        var rule = Orchd_V1_OutboundRule()
        rule.id = "user-1"
        rule.kind = "destination"
        rule.on = true
        rule.summary = "no space's terms"
        wire.rules = [rule]
        wire.revision = 3
        wire.setAt = 1_800_000_000
        wire.setBy = "operator (process 9)"
        wire.termsLayer = "on"
        var change = Orchd_V1_OutboundRuleChange()
        change.revision = 3
        change.at = 1_800_000_000
        change.action = "add"
        change.ruleID = "user-1"
        wire.history = [change]
        wire.kinds = ["command"]
        let s = DaemonClient.outboundRules(wire)
        #expect(s.rules == [OutboundRule(id: "user-1", kind: "destination", on: true, summary: "no space's terms")])
        #expect(s.revision == 3 && s.setBy == "operator (process 9)" && s.setAt == Date(timeIntervalSince1970: 1_800_000_000))
        #expect(s.history.first?.ruleID == "user-1" && s.kinds == ["command"] && !s.termsOff)
    }

}

@Suite struct OutboundRulesClientTests {
    /// DaemonClient lists and changes the rules over gRPC, sending the
    /// value only in the change request.
    @Test func daemonClientOverGRPC() async throws {
        var service = FakeOrchdService()
        var rule = Orchd_V1_OutboundRule()
        rule.id = "plain_variables"
        rule.kind = "baseline"
        rule.baseline = true
        service.outboundRules.rules = [rule]
        service.outboundRules.termsLayer = "on"
        service.changeOutboundRule.ruleID = "user-1"
        service.changeOutboundRule.changed = true
        service.changeOutboundRule.revision = 1
        service.changeOutboundRule.current = service.outboundRules
        let recorder = service.recorder
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let listed = try await client.outboundRules()
            #expect(listed.rules.map(\.id) == ["plain_variables"])
            let r = try await client.changeOutboundRule(action: "add", id: "", kind: "command", value: "glab")
            #expect(r.ruleID == "user-1" && r.changed && r.revision == 1 && r.current?.rules.count == 1)
            let sent = try #require(recorder.last(Orchd_V1_ChangeOutboundRuleRequest.self))
            #expect(sent.action == "add" && sent.kind == "command" && sent.value == "glab")
        }
    }
}
