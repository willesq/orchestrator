import Foundation

/// The enforcement switches and the directory validator, for the Spaces
/// section and the menu bar panel.
extension DaemonClient: SpacesAPI {
    public func enforcement() async throws -> EnforcementState {
        let r = try await orchd.getEnforcement(Orchd_V1_GetEnforcementRequest())
        return EnforcementState(switches: r.switches.map(Self.enforcementSwitch), error: r.error)
    }

    public func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        var request = Orchd_V1_SetEnforcementRequest()
        request.layer = layer
        request.state = state
        request.forSeconds = forSeconds
        let r = try await orchd.setEnforcement(request)
        return EnforcementChangeResult(current: Self.enforcementSwitch(r.current), changed: r.changed, notes: r.notes)
    }

    public func explainPlacement(target: String) async throws -> PlacementExplanation {
        var request = Orchd_V1_ExplainPlacementRequest()
        request.target = target
        return Self.placement(try await orchd.explainPlacement(request))
    }

    public func spaceLogins() async throws -> [SpaceLogin] {
        let r = try await orchd.listSpaces(Orchd_V1_ListSpacesRequest())
        return r.logins.map { SpaceLogin(space: $0.space, gh: Self.loginState($0.gh), claude: Self.loginState($0.claude)) }
    }

    static func loginState(_ s: Orchd_V1_LoginState) -> LoginState {
        LoginState(own: s.own, dir: s.dir, state: s.state, account: s.account, storage: s.storage, detail: s.detail)
    }

    static func enforcementSwitch(_ s: Orchd_V1_EnforcementSwitch) -> EnforcementSwitch {
        EnforcementSwitch(
            layer: s.layer, title: s.title, state: s.state, defaultState: s.defaultState, states: s.states,
            relaxed: s.relaxed, setAt: s.setAt > 0 ? Date(timeIntervalSince1970: TimeInterval(s.setAt)) : nil,
            setBy: s.setBy, until: s.until > 0 ? Date(timeIntervalSince1970: TimeInterval(s.until)) : nil,
            revertsTo: s.revertsTo, problem: s.problem
        )
    }

    static func placement(_ e: Orchd_V1_PlacementExplanation) -> PlacementExplanation {
        PlacementExplanation(
            targetKind: e.targetKind,
            roots: e.roots.map { PlacementRoot(root: $0.root, space: $0.space, kind: $0.kind) },
            remote: e.hasRemote ? PlacementRemote(owner: e.remote.owner, repo: e.remote.repo, visibility: e.remote.visibility,
                                                  space: e.remote.space, kind: e.remote.kind, effect: e.remote.effect) : nil,
            rule: e.rule, reason: e.reason, outcome: e.outcome, space: e.space,
            sessionSpace: e.sessionSpace, sessionReads: e.sessionReads
        )
    }
}
