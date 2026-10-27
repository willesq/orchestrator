import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private struct Down: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private struct Busy: Error, CustomStringConvertible {
    var description: String {
        #"RPCError(code: aborted, message: "a safety-net install is already running (install, started 3s ago); this request was not queued")"#
    }
}

private final class FakeSafetyNet: SafetyNetAPI, @unchecked Sendable {
    private let lock = NSLock()
    var current = SafetyNetState(state: "missing")
    var result = SafetyNetState(state: "ok", ok: true, action: SafetyNetAction(ok: true))
    var fail = false
    var installError: (any Error)?
    /// Held open until released, so a test can watch an action run.
    var gate: AsyncStream<Void>?
    private(set) var installs: [Bool] = []
    private(set) var polls = 0

    func safetyNet() async throws -> SafetyNetState {
        if fail { throw Down() }
        return lock.withLock {
            polls += 1
            return current
        }
    }

    func installSafetyNet(rulesOnly: Bool) async throws -> SafetyNetState {
        lock.withLock { installs.append(rulesOnly) }
        if let gate { for await _ in gate { break } }
        if let installError { throw installError }
        return result
    }
}

@MainActor
struct SafetyNetModelTests {
    @Test func loadShowsStateOrError() async {
        let api = FakeSafetyNet()
        let model = SafetyNetModel(api: api)
        await model.load()
        #expect(model.state?.state == "missing" && model.lastError == nil)
        api.fail = true
        await model.load()
        #expect(model.lastError == "unavailable")
        #expect(model.state?.state == "missing")
    }

    @Test func installReportsSuccess() async {
        let api = FakeSafetyNet()
        let model = SafetyNetModel(api: api)
        await model.install()
        #expect(api.installs == [false])
        #expect(model.state?.ok == true && model.lastError == nil && model.busy == false)
        #expect(model.message?.contains("Restart running Claude Code sessions") == true)
        await model.repairRules()
        #expect(api.installs == [false, true])
        #expect(model.message?.contains("Repaired the rules") == true)
        await model.upgrade()
        #expect(model.message?.hasPrefix("Upgraded") == true)
    }

    @Test func failedActionShowsItsError() async {
        let api = FakeSafetyNet()
        api.result = SafetyNetState(state: "missing", action: SafetyNetAction(ok: false, error: "install cc-safety-net: exit status 1"))
        let model = SafetyNetModel(api: api)
        await model.install()
        #expect(model.lastError == "install cc-safety-net: exit status 1" && model.message == nil)
        api.result = SafetyNetState(state: "missing", action: SafetyNetAction(ok: false))
        await model.install()
        #expect(model.lastError == "The install did not finish.")
    }

    @Test func refusalShowsOrchdsReason() async {
        let api = FakeSafetyNet()
        api.installError = Busy()
        let model = SafetyNetModel(api: api)
        await model.install()
        #expect(model.lastError?.contains("already running") == true)
        #expect(model.lastError?.contains("not queued") == true)
    }

    @Test func progressIsFollowedWhileRunning() async throws {
        let api = FakeSafetyNet()
        api.current = SafetyNetState(state: "missing", action: SafetyNetAction(running: true, step: "install cc-safety-net@orch-pinned"))
        let (gate, open) = AsyncStream<Void>.makeStream()
        api.gate = gate
        let model = SafetyNetModel(api: api)
        let action = Task { await model.run(rulesOnly: false, verb: "Installed", poll: .milliseconds(10)) }
        let deadline = ContinuousClock.now + .seconds(5)
        while model.state?.action.running != true, ContinuousClock.now < deadline {
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(model.busy)
        #expect(model.state?.action.step == "install cc-safety-net@orch-pinned")
        // A second click while one runs does nothing.
        await model.install()
        #expect(api.installs.count == 1)
        open.yield()
        open.finish()
        await action.value
        #expect(model.state?.ok == true && !model.busy)
    }
}

struct SafetyNetStateTests {
    @Test func badgesAndActions() {
        let okRules = SafetyNetRules(ok: true)
        let cases: [(SafetyNetState, String, SafetyNetState.Tone, Bool, Bool, Bool)] = [
            (SafetyNetState(state: "ok", rules: okRules), "Protected", .good, false, false, false),
            (SafetyNetState(state: "ok"), "Rules incomplete", .warning, false, false, true),
            (SafetyNetState(state: "missing", rules: okRules), "Not installed", .bad, true, false, false),
            (SafetyNetState(state: "legacy", rules: okRules), "Old copy installed", .warning, true, false, false),
            (SafetyNetState(state: "outdated", rules: okRules), "Update available", .warning, false, true, false),
            (SafetyNetState(state: "disabled", rules: okRules), "Disabled", .warning, true, false, false),
            (SafetyNetState(state: "?", rules: okRules), "Unknown", .neutral, false, false, false),
        ]
        for (state, label, tone, install, upgrade, repair) in cases {
            #expect(state.badge.label == label)
            #expect(state.badge.tone == tone)
            #expect(state.canInstall == install && state.canUpgrade == upgrade && state.canRepairRules == repair)
        }
    }

    @Test func ruleAndRulebookHelpers() {
        #expect(SafetyNetRule(name: "a", command: "git", subcommand: "push").commandLine == "git push")
        #expect(SafetyNetRule(name: "b", command: "soffice").commandLine == "soffice")
        let rb = SafetyNetRulebook(source: "o/r#main/team")
        #expect(rb.name == "o/r#main/team" && !rb.isOrch && rb.id == "o/r#main/team")
        #expect(SafetyNetRulebook(source: "orch-guards", owner: "orch").isOrch)
        #expect(SafetyNetInstall(pluginID: "safety-net@cc-marketplace").id == "safety-net@cc-marketplace/user")
        #expect(!SafetyNetAction().hasRun)
        #expect(SafetyNetAction(log: "one\ntwo\u{202E}\n").displayLog == "one\ntwo\\u{202E}\n")
        #expect(SafetyNetAction(startedAt: Date()).hasRun)
    }

    @Test func clientConvertsOverGRPC() async throws {
        var service = FakeOrchdService()
        var s = Orchd_V1_SafetyNetState()
        s.state = "legacy"
        s.pinnedVersion = "2.6.10"
        s.pinnedSha = "c745205d0ed4684cea070e382177d48c21b11154"
        s.pinnedReleasedOn = "2026-10-12"
        s.installedVersion = "2.6.10"
        s.installedSha = "c745205d0ed4684cea070e382177d48c21b11154"
        var legacy = Orchd_V1_SafetyNetInstall()
        legacy.id = "safety-net@cc-marketplace"
        legacy.scope = "user"
        legacy.version = "0.7.1"
        s.legacy = [legacy]
        s.errors = ["installed_plugins.json: bad"]
        var rule = Orchd_V1_SafetyNetRule()
        rule.name = "git-push-default-branch"
        rule.command = "git"
        rule.subcommand = "push"
        rule.blockArgs = ["main", "--all"]
        rule.reason = "Never push to the default branch."
        rule.off = true
        var rb = Orchd_V1_SafetyNetRulebook()
        rb.source = "orch-guards"
        rb.name = "orch-guards"
        rb.owner = "orch"
        rb.version = "1.0.0"
        rb.description_p = "d"
        rb.rules = [rule]
        s.rules.ok = true
        s.rules.configPath = "/h/.cc-safety-net/rules/rule.json"
        s.rules.configExists = true
        s.rules.referenced = true
        s.rules.rtkWrapper = true
        s.rules.rulebookCurrent = true
        s.rules.rulebooks = [rb]
        s.action.running = false
        s.action.startedAt = 1_700_000_000
        s.action.finishedAt = 1_700_000_030
        s.action.ok = true
        s.action.step = "install"
        s.action.log = "done"
        service.safetyNet = s

        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let state = try await client.safetyNet()
            #expect(state.state == "legacy" && state.pinnedVersion == "2.6.10" && state.pinnedReleasedOn == "2026-10-12")
            #expect(state.installedSHA == s.installedSha && state.pinnedSHA == s.pinnedSha && state.installedVersion == "2.6.10")
            #expect(state.legacy == [SafetyNetInstall(pluginID: "safety-net@cc-marketplace", scope: "user", version: "0.7.1")])
            #expect(state.errors == ["installed_plugins.json: bad"])
            #expect(state.rules == SafetyNetRules(
                ok: true, configPath: "/h/.cc-safety-net/rules/rule.json", configExists: true, referenced: true,
                rtkWrapper: true, rulebookCurrent: true,
                rulebooks: [SafetyNetRulebook(source: "orch-guards", name: "orch-guards", owner: "orch", version: "1.0.0",
                                              description: "d",
                                              rules: [SafetyNetRule(name: "git-push-default-branch", command: "git",
                                                                    subcommand: "push", blockArgs: ["main", "--all"],
                                                                    reason: "Never push to the default branch.", off: true)])]))
            #expect(state.action == SafetyNetAction(startedAt: Date(timeIntervalSince1970: 1_700_000_000),
                                                    finishedAt: Date(timeIntervalSince1970: 1_700_000_030),
                                                    step: "install", ok: true, log: "done"))

            _ = try await client.installSafetyNet(rulesOnly: true)
            #expect(service.recorder.last(Orchd_V1_InstallSafetyNetRequest.self)?.rulesOnly == true)
        }
    }
}
