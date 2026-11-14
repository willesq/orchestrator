import Foundation
import Testing
@testable import OrchKit

private struct LoginsFake: SpacesAPI {
    var logins: [SpaceLogin] = []
    var fail: (any Error)?

    func enforcement() async throws -> EnforcementState { EnforcementState(switches: []) }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }
    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
    func spaceLogins() async throws -> [SpaceLogin] {
        if let fail { throw fail }
        return logins
    }
}

private struct NoLogins: SpacesAPI {
    func enforcement() async throws -> EnforcementState { EnforcementState(switches: []) }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }
    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
}

/// SP-074.3.a3 for the app: the Spaces section reads each space's login
/// source and state, in one-login and two-login layouts.
@MainActor
@Suite struct SpaceLoginsTests {
    @Test func oneLogin() async {
        let shared = LoginState(state: "logged in", account: "willesq", storage: "keyring")
        let claude = LoginState(state: "logged in", account: "claude.ai")
        let model = SpaceLoginsModel(api: LoginsFake(logins: [
            SpaceLogin(space: "acme", gh: shared, claude: claude),
            SpaceLogin(space: "personal", gh: shared, claude: claude),
        ]))
        await model.load()
        #expect(model.logins?.count == 2)
        #expect(model.logins?.allSatisfy { !$0.needsAttention } == true)
        #expect(shared.summary(defaultName: "Default login", ownName: "Own profile")
            == "Default login, logged in as willesq (token in keyring)")
    }

    @Test func twoLogins() async {
        let own = LoginState(own: true, dir: "/Users/u/.config/gh-acme", state: "logged in", account: "work-acme", storage: "file")
        let out = LoginState(own: true, dir: "/Users/u/.claude-acme", state: "not logged in", detail: "log in with /login")
        let model = SpaceLoginsModel(api: LoginsFake(logins: [
            SpaceLogin(space: "acme", gh: own, claude: out),
            SpaceLogin(space: "globex", gh: LoginState(state: "logged in", account: "willesq"), claude: LoginState(state: "unknown")),
        ]))
        await model.load()
        let acme = model.logins?.first { $0.space == "acme" }
        #expect(acme?.needsAttention == true)
        #expect(acme?.gh.summary(defaultName: "Default login", ownName: "Own profile") == "Own profile, logged in as work-acme (token in file)")
        #expect(model.logins?.first { $0.space == "globex" }?.needsAttention == false)
        #expect(LoginState(own: true, state: "refused").summary(defaultName: "d", ownName: "o") == "o, refused")
    }

    @Test func refusalAndOlderSources() async {
        let refused = SpaceLoginsModel(api: LoginsFake(fail: CancellationError()))
        await refused.load()
        #expect(refused.logins == nil)
        #expect(refused.lastError != nil)
        let none = SpaceLoginsModel(api: NoLogins())
        await none.load()
        #expect(none.logins == [])
        #expect(EnforcementModel(api: NoLogins()).logins.logins == nil)
    }
}
