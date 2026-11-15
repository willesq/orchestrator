import Foundation

/// The outbound terms check's rules (spaces SP-075).
extension DaemonClient {
    public func outboundRules() async throws -> OutboundRulesState {
        Self.outboundRules(try await orchd.listOutboundRules(Orchd_V1_ListOutboundRulesRequest()))
    }

    public func changeOutboundRule(action: String, id: String, kind: String, value: String) async throws -> OutboundRuleChangeResult {
        var request = Orchd_V1_ChangeOutboundRuleRequest()
        request.action = action
        request.id = id
        request.kind = kind
        request.value = value
        let r = try await orchd.changeOutboundRule(request)
        return OutboundRuleChangeResult(ruleID: r.ruleID, changed: r.changed, revision: r.revision,
                                        current: r.hasCurrent ? Self.outboundRules(r.current) : nil)
    }

    static func outboundRules(_ r: Orchd_V1_OutboundRules) -> OutboundRulesState {
        func date(_ s: Int64) -> Date? { s > 0 ? Date(timeIntervalSince1970: TimeInterval(s)) : nil }
        return OutboundRulesState(
            rules: r.rules.map { OutboundRule(id: $0.id, kind: $0.kind, on: $0.on, loosens: $0.loosens, summary: $0.summary, baseline: $0.baseline) },
            revision: r.revision, setAt: date(r.setAt), setBy: r.setBy, problem: r.problem, termsLayer: r.termsLayer,
            history: r.history.map { OutboundRuleChangeEntry(revision: $0.revision, at: date($0.at), by: $0.by, action: $0.action, ruleID: $0.ruleID) },
            kinds: r.kinds
        )
    }
}
