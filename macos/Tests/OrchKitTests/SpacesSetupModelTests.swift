import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private let applied = """
{"spaces":[{"github_orgs":["oss-org"],"kind":"public","name":"public"}]}
"""

/// SP-052.1: the wizard's flow, cancelling, applying and the click count.
@MainActor
@Suite struct SpacesSetupModelTests {
    private func wizard(_ api: FakeSetupAPI, presence: Presence = Presence()) -> SpacesSetupModel {
        SpacesSetupModel(api: api, home: setupHome, folders: ["\(setupHome)/work", "\(setupHome)/code"],
                         authorize: { try await presence.check($0) })
    }

    private func walk(_ m: SpacesSetupModel, to step: SpacesSetupModel.Step) async {
        while m.step < step { await m.next() }
    }

    /// SP-052.1 a2: cancelling at any step leaves orchd's applied policy,
    /// switches and retention unchanged, and leaves no drift behind.
    @Test(arguments: SpacesSetupModel.Step.allCases)
    func cancellingAtAnyStepChangesNothingApplied(_ step: SpacesSetupModel.Step) async throws {
        let api = FakeSetupAPI(applied: applied)
        let m = wizard(api)
        await m.load()
        m.setGuard("strict", for: "guard")
        m.setRetention("30d")
        await walk(m, to: step)
        #expect(m.step == step)
        let note = await m.cancel()
        #expect(note == nil)
        #expect(api.appliedJSON == applied)
        #expect(api.applyCalls.isEmpty)
        #expect(api.enforcementCalls.isEmpty)
        #expect(api.retentionCalls.isEmpty)
        #expect(!api.drift)
    }

    /// With nothing applied yet, a draft saved for review stays, and the
    /// user is told it enforces nothing.
    @Test func cancellingTheFirstSetupKeepsItsDraftUnapplied() async {
        let api = FakeSetupAPI()
        let m = wizard(api)
        await m.load()
        await walk(m, to: .review)
        #expect(m.draft != nil)
        let note = await m.cancel()
        #expect(note?.contains("changes nothing until it is applied") == true)
        #expect(api.appliedJSON == "")
        #expect(api.applyCalls.isEmpty)
        #expect(await m.cancel() != nil)
    }

    /// An agent's pending draft is not replaced by the applied policy on
    /// cancel: setup cannot tell whose it should restore.
    @Test func cancellingOverAPendingDraftSaysSo() async {
        let api = FakeSetupAPI(applied: applied, pendingDraft: "{\"spaces\":[]}")
        let m = wizard(api)
        await m.load()
        #expect(m.replacedDraft?.revision == 2)
        await walk(m, to: .review)
        #expect(m.draft?.revision == 3)
        #expect(await m.cancel()?.contains("Draft revision 3") == true)
        #expect(api.appliedJSON == applied)
    }

    /// SP-052.1 a3: on the fixture machine, setup takes 8 clicks from the
    /// Spaces pane, the scan excluded: Set Up Spaces, Continue four times,
    /// Apply, the Touch ID prompt and Done.
    @Test func fixtureSetupTakesAtMostTenClicks() async throws {
        let api = FakeSetupAPI()
        let presence = Presence()
        var clicks = 1 // "Set Up Spaces…" opens the wizard; the scan starts by itself.
        let m = wizard(api, presence: presence)
        await m.load()
        #expect(api.scannedFolders.count == 1)
        for _ in 0..<4 {
            await m.next()
            clicks += 1
        }
        #expect(m.step == .review)
        #expect(m.canApply)
        await m.apply()
        clicks += 1 + presence.asked // Apply, then Touch ID
        #expect(m.outcome?.failed == [])
        clicks += 1 // Done
        #expect(clicks == 8)
        #expect(clicks <= 10)

        // What was applied is the fixture's four spaces, guard at its default.
        let doc = try #require(try JSONSerialization.jsonObject(with: Data(api.appliedJSON.utf8)) as? [String: Any])
        let names = (doc["spaces"] as? [[String: Any]])?.compactMap { $0["name"] as? String }.sorted()
        #expect(names == ["acme", "globex", "personal", "public"])
        #expect(api.enforcementCalls.map(\.0) == ["guard"])
        #expect(api.enforcementCalls.map(\.1) == ["block-other-jobs"])
        #expect(api.retentionCalls.isEmpty)
        #expect(m.outcome?.done.first == "Applied policy revision 1.")
    }

    /// Cancel re-saves the applied policy only while setup's draft is the
    /// newest and the applied revision is the one setup read.
    @Test func cancelNeverHidesANewerDraftOrRevivesAnOldPolicy() async {
        let racing = FakeSetupAPI(applied: applied)
        racing.agentDraftAfterSave = true
        let r = wizard(racing)
        await r.load()
        await walk(r, to: .review)
        #expect(await r.cancel()?.contains("stays saved") == true)
        #expect(racing.revisionCount == 3)

        let moved = FakeSetupAPI(applied: applied)
        let m = wizard(moved)
        await m.load()
        await walk(m, to: .enforcement)
        moved.applyElsewhere("{\"spaces\":[]}")
        await m.next()
        #expect(m.reviewError?.contains("was applied while setup was open") == true)
        #expect(!m.canApply)
        let before = moved.revisionCount
        #expect(await m.cancel() != nil)
        #expect(moved.revisionCount == before)
        #expect(moved.appliedJSON == "{\"spaces\":[]}")
    }

    @Test func anUnreadableAppliedPolicyIsNeverReplaced() async {
        let api = FakeSetupAPI(applied: "{\"spaces\":null}")
        let m = wizard(api)
        await m.load()
        #expect(m.loadError == nil)
        await walk(m, to: .review)
        #expect(m.plan?.unreadableBase == true)
        #expect(m.reviewError?.contains("could not be read") == true)
        #expect(m.draft == nil)
        #expect(!m.canApply)
    }

    /// A first setup never starts a layer below where the user put it.
    @Test func firstSetupNeverLowersASwitch() async {
        let api = FakeSetupAPI()
        api.switches[1].state = "strict"
        let m = wizard(api)
        await m.load()
        #expect(m.guardChoices["guard"] == "strict")
        #expect(m.guardChoices["memory"] == "on")
        #expect(m.guardChanges.isEmpty)
        #expect(SpacesSetupModel.stricter("off", "", in: ["off", "on"]) == "off")
        #expect(SpacesSetupModel.stricter("weird", "on", in: ["off", "on"]) == "weird")
        // orchd lists the default first: a first setup still raises a layer.
        #expect(SpacesSetupModel.stricter("log", "block-other-jobs", in: ["block-other-jobs", "off", "log", "strict"]) == "block-other-jobs")
        #expect(SpacesSetupModel.stricter("off", "on", in: ["on", "off"]) == "on")
        #expect(SpacesSetupModel.stricter("off", "on", in: ["off"]) == "off")
    }

    @Test func applyNeedsTheDeviceOwner() async {
        let api = FakeSetupAPI()
        let presence = Presence()
        presence.refuse = true
        let m = wizard(api, presence: presence)
        await m.load()
        await walk(m, to: .review)
        await m.apply()
        #expect(presence.asked == 1)
        #expect(m.applyError == "Not applied: refused")
        #expect(api.applyCalls.isEmpty && api.enforcementCalls.isEmpty)
        #expect(m.outcome == nil)
    }

    @Test func aRefusedPolicyChangesNothingElse() async {
        let api = FakeSetupAPI()
        // orchd refuses a caller that is not the operator (INV-06, D-71).
        api.failApply = RPCError(code: .permissionDenied, message: "only the operator may apply")
        let m = wizard(api)
        await m.load()
        m.setRetention("3d")
        await walk(m, to: .review)
        await m.apply()
        #expect(m.applyError == "Not applied: only the operator may apply")
        #expect(api.enforcementCalls.isEmpty && api.retentionCalls.isEmpty)
        #expect(m.outcome == nil)
    }

    @Test func switchesAndRetentionAloneApplyWithoutADraft() async {
        let api = FakeSetupAPI(applied: applied)
        api.scanResult = SpacesScan(checkouts: [ScannedCheckout(path: "/x/o", owner: "oss-org", repo: "o", visibility: "public")])
        let m = wizard(api)
        await m.load()
        m.setRetention("30d")
        m.setRetention("never")
        m.setGuard("bogus", for: "guard")
        m.setGuard("off", for: "missing")
        // A rerun starts each switch at its current state.
        #expect(m.guardChanges.isEmpty)
        m.setGuard("block-other-jobs", for: "guard")
        await walk(m, to: .review)
        #expect(m.plan?.changesPolicy == false)
        #expect(m.draft == nil)
        #expect(m.canApply)
        await m.apply()
        #expect(api.applyCalls.isEmpty)
        #expect(api.retentionCalls == ["30d"])
        #expect(m.outcome?.done.contains { $0.contains("30 days after a session ends") && $0.contains("restarts") } == true)
    }

    @Test func nothingToChangeCannotApply() async {
        let api = FakeSetupAPI(applied: applied)
        api.scanResult = SpacesScan()
        api.switches = [EnforcementSwitch(layer: "memory", state: "on", defaultState: "on", states: ["off", "on"])]
        let m = wizard(api)
        await m.load()
        await walk(m, to: .review)
        #expect(!m.hasChanges)
        #expect(!m.canApply)
        await m.apply()
        #expect(m.outcome == nil)
    }

    @Test func problemsAndANewerDraftBlockApply() async {
        let api = FakeSetupAPI()
        api.problems = [SpacesProblem(rule: "bad_binding", space: "acme", message: "bad")]
        let m = wizard(api)
        await m.load()
        await walk(m, to: .review)
        #expect(m.reviewProblems.map(\.message) == ["bad"])
        #expect(!m.canApply)

        let racing = FakeSetupAPI()
        racing.agentDraftAfterSave = true
        let r = wizard(racing)
        await r.load()
        await walk(r, to: .review)
        #expect(r.reviewError?.contains("newer draft (rev 2)") == true)
        #expect(!r.canApply)
        // Going back and reviewing again reuses the saved draft.
        r.back()
        #expect(r.step == .enforcement)
        await r.next()
        #expect(racing.revisionCount == 2)
    }

    @Test func choicesFeedThePlan() async {
        let api = FakeSetupAPI()
        let m = wizard(api)
        await m.load()
        let oss = m.groups.first { $0.id == "oss-org" }!
        m.setKind(.leaveOut, for: "oss-org")
        #expect(m.kind(of: oss) == .leaveOut)
        #expect(m.space(of: oss) == nil)
        m.setProfile("~/gh/acme-work", for: "acme")
        #expect(m.newJobs.map(\.space.ghConfigDir) == ["~/gh/acme-work", "~/.config/gh-globex"])
        m.setKind(.job, for: "nobody")
        #expect(m.kinds["nobody"] == nil)
        m.addFolder("relative")
        m.addFolder("/elsewhere")
        m.addFolder("/elsewhere")
        #expect(m.folders == ["\(setupHome)/work", "\(setupHome)/code", "/elsewhere"])
        m.removeFolder("/elsewhere")
        await m.runScan()
        #expect(api.scannedFolders.last == ["\(setupHome)/work", "\(setupHome)/code"])
        // A rescan keeps the user's choices for owners still found.
        #expect(m.kind(of: oss) == .leaveOut)
        #expect(m.guardChanges.map(\.state) == ["block-other-jobs"])
        #expect(m.retentionChange == nil)
    }

    @Test func aBoundOwnerIsLocked() async {
        let api = FakeSetupAPI(applied: applied)
        let m = wizard(api)
        await m.load()
        let oss = m.groups.first { $0.id == "oss-org" }!
        #expect(oss.locked)
        m.setKind(.job, for: "oss-org")
        #expect(m.kind(of: oss) == .open)
    }

    @Test func readFailuresStopOrReport() async {
        let api = FakeSetupAPI()
        api.failDiff = SetupRefused()
        let m = wizard(api)
        await m.load()
        #expect(m.loadError == "refused")
        #expect(!m.canContinue)
        #expect(api.scannedFolders.isEmpty)
        #expect(await m.cancel() == nil)

        let scanFails = FakeSetupAPI()
        scanFails.failScan = SetupRefused()
        let s = wizard(scanFails)
        await s.load()
        #expect(s.scanError == "refused")
        #expect(s.scan == nil && s.plan == nil)
        #expect(!s.canContinue)
        await s.next()
        #expect(s.step == .scan)
    }

    @Test func defaultFoldersAreTheUsualOnesThatExist() throws {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent("sp052-\(UUID().uuidString)").path
        try FileManager.default.createDirectory(atPath: home + "/code", withIntermediateDirectories: true)
        try FileManager.default.createDirectory(atPath: home + "/Developer", withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: home + "/work", contents: Data())
        defer { try? FileManager.default.removeItem(atPath: home) }
        let m = SpacesSetupModel(api: FakeSetupAPI(), home: home)
        #expect(m.folders == [home + "/code", home + "/Developer"])
        #expect(SpacesSetupModel.Step.allCases.map(\.title) == ["Scan", "Spaces", "Recall", "Guard", "Review"])
    }

    @Test func loginStatusComesFromOrchdOrSaysHowToLogIn() async {
        let api = FakeSetupAPI()
        api.logins = [SpaceLogin(space: "acme", gh: LoginState(own: true, dir: "\(setupHome)/.config/gh-acme", state: "logged in", account: "me-acme"),
                                 claude: LoginState())]
        api.scanResult.accounts.append(ScannedAccount(profile: "globex", login: "", error: "not logged in"))
        let m = wizard(api)
        await m.load()
        let jobs = Dictionary(uniqueKeysWithValues: m.newJobs.map { ($0.space.name, m.loginStatus(for: $0.space)) })
        #expect(jobs["acme"] == SetupLoginStatus(state: .loggedIn, text: "logged in as me-acme"))
        #expect(jobs["globex"]?.state == .notLoggedIn)
        #expect(jobs["globex"]?.command == "GH_CONFIG_DIR=~/.config/gh-globex gh auth login")

        let s = { (logins: [SpaceLogin], accounts: [ScannedAccount]) in
            SetupLoginStatus.of(space: "j", dir: "~/g", logins: logins, accounts: accounts, home: "/h")
        }
        #expect(s([SpaceLogin(space: "j", gh: LoginState(own: true, dir: "/h/g", state: "not logged in"), claude: LoginState())], []).state
            == .notLoggedIn)
        #expect(s([SpaceLogin(space: "j", gh: LoginState(own: true, dir: "/h/g", state: "logged in"), claude: LoginState())], []).text
            == "logged in")
        #expect(s([], [ScannedAccount(profile: "j", login: "x")]).text == "logged in as x")
        #expect(s([], []).state == .notChecked)
        #expect(SetupLoginStatus.loginCommand("~/my gh") == "GH_CONFIG_DIR=\"$HOME\"/'my gh' gh auth login")
        #expect(SetupLoginStatus.loginCommand("/a b/it's") == "GH_CONFIG_DIR='/a b/it'\\''s' gh auth login")
        #expect(SetupLoginStatus.loginCommand("~other/gh") == "GH_CONFIG_DIR='~other/gh' gh auth login")
        #expect(SetupLoginStatus.loginCommand("/caf\u{e9}") == "GH_CONFIG_DIR='/caf\u{e9}' gh auth login")
        #expect(SetupLoginStatus.loginCommand("/a\u{202E}b") == nil)
        #expect(s([], [ScannedAccount(profile: "j", login: "", error: "x")]).text == "not logged in")
    }

    @Test func retentionLabelsAndUnavailableSources() async {
        #expect(CommandLogRetention.label("1d") == "1 day after a session ends")
        #expect(CommandLogRetention.label("7d") == "7 days after a session ends")
        #expect(CommandLogRetention.label("forever") == "forever")
        #expect(SpacesSetupUnavailable().errorDescription?.contains("orchd") == true)

        // A source with no setup reports it as unavailable.
        struct Bare: SpacesSetupAPI {
            func enforcement() async throws -> EnforcementState { EnforcementState() }
            func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
                throw CancellationError()
            }
            func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
        }
        let bare = Bare()
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.scanSpaces(folders: [], offline: true) }
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.saveSpacesDraft(policyJSON: "{}") }
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.diffSpaces() }
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.applySpaces(reviewedRevision: 1) }
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.commandLogRetention() }
        await #expect(throws: SpacesSetupUnavailable.self) { try await bare.setCommandLogRetention("3d") }
        let m = SpacesSetupModel(api: bare, home: setupHome, folders: [])
        await m.load()
        #expect(m.loadError?.contains("orchd") == true)
    }
}
