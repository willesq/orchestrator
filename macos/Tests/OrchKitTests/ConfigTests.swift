import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private struct Refused: Error, CustomStringConvertible {
    var description: String { #"RPCError(code: invalidArgument, message: "queue.worker.max_concurrent: must be from 0 to 50")"# }
}

private let parallel = ConfigField(key: "queue.worker.max_concurrent", section: "Tasks", label: "Parallel tasks", type: .int,
                                   value: "1", defaultValue: "1", range: 0...50)
private let mode = ConfigField(key: "queue.worker.permission_mode", section: "Tasks", label: "Permission mode", type: .enum,
                               value: "\"auto\"", defaultValue: "\"auto\"", options: ["auto", "plan"], sensitive: true)

private final class FakeConfig: ConfigAPI, @unchecked Sendable {
    private let lock = NSLock()
    var state = ConfigState(fields: [parallel, mode], path: "/h/config.yaml")
    var fail: (any Error)?
    var writes: [(values: [String: String], confirmed: Bool)] = []
    var loads = 0

    func config() async throws -> ConfigState {
        try lock.withLock {
            loads += 1
            if let fail { throw fail }
            return state
        }
    }

    func setConfig(_ values: [String: String], confirmSensitive: Bool) async throws -> ConfigState {
        try lock.withLock {
            if let fail { throw fail }
            writes.append((values, confirmSensitive))
            for (k, v) in values {
                if let i = state.fields.firstIndex(where: { $0.key == k }) {
                    state.fields[i].value = v
                    state.fields[i].pendingRestart = true
                }
            }
            state.restartRequired = true
            return state
        }
    }
}

@MainActor
private func loaded(_ api: FakeConfig, restart: @escaping @Sendable () async throws -> Void = {}) async -> ConfigModel {
    let model = ConfigModel(api: api, settleDelay: .milliseconds(1), restartDaemon: restart)
    await model.load()
    return model
}

@Suite("ConfigField")
struct ConfigFieldTests {
    @Test("Parallel tasks stays within the hard limit before anything is sent",
          arguments: [("0", "0"), ("1", "1"), ("50", "50"), (" 5 ", "5")])
    func accepted(typed: String, want: String) throws {
        #expect(try parallel.json(fromText: typed).get() == want)
    }

    @Test("Out of range or not a whole number is refused", arguments: ["51", "-1", "1.5", "five", ""])
    func refused(typed: String) {
        guard case .failure = parallel.json(fromText: typed) else {
            Issue.record("\(typed) was accepted")
            return
        }
    }

    @Test("Each type reads and writes its JSON")
    func types() throws {
        let list = ConfigField(key: "l", label: "L", type: .list, value: #"["a","b"]"#, defaultValue: "[]")
        #expect(list.multilineText == "a\nb")
        #expect(try list.json(fromText: "x\n\n y \n").get() == #"["x","y"]"#)
        #expect(!list.isDefault)

        let map = ConfigField(key: "m", label: "M", type: .map, value: #"{"a":1}"#, defaultValue: #"{ "a": 1 }"#)
        #expect(map.isDefault)
        #expect(map.multilineText.contains("\"a\" : 1"))
        #expect(try map.json(fromText: #"{"b":2}"#).get() == #"{"b":2}"#)
        if case .success = map.json(fromText: "[1]") { Issue.record("a list passed as a map") }
        if case .success = map.json(fromText: "{") { Issue.record("broken JSON passed as a map") }

        let flag = ConfigField(key: "b", label: "B", type: .bool, value: "true", defaultValue: "false")
        #expect(flag.boolValue)
        #expect(try flag.json(fromText: "false").get() == "false")

        let number = ConfigField(key: "f", label: "F", type: .float, value: "0.5", defaultValue: "0.5", range: 0...1)
        #expect(number.text == "0.5")
        #expect(try number.json(fromText: "0.25").get() == "0.25")
        if case .success = number.json(fromText: "2") { Issue.record("out of range passed") }
        if case .success = number.json(fromText: "x") { Issue.record("text passed as a number") }
        if case .success = number.json(fromText: "inf") { Issue.record("infinity passed") }

        let text = ConfigField(key: "s", label: "S", type: .duration, value: "\"10m\"", defaultValue: "\"10m\"")
        #expect(text.text == "10m")
        #expect(try text.json(fromText: " 5/m ").get() == "\"5/m\"")
        #expect(ConfigField(key: "x", value: "not json", defaultValue: "not json").isDefault)
        #expect(ConfigField(key: "x", value: "not json").text == "not json")
        #expect(ConfigJSON.pretty("not json") == "not json")
    }
}

@Suite("ConfigModel")
@MainActor
struct ConfigModelTests {
    @Test("Loads and groups by section")
    func load() async {
        let model = await loaded(FakeConfig())
        #expect(model.fields(section: "Tasks").count == 2)
        #expect(model.fields(section: "Docs").isEmpty)
        #expect(model.pendingRestartCount == 0)
        #expect(model.lastError == nil)
    }

    @Test("A failed load says why, and a good one clears it")
    func loadFailure() async {
        let api = FakeConfig()
        api.fail = Refused()
        let model = await loaded(api)
        #expect(model.lastError?.contains("from 0 to 50") == true)
        api.fail = nil
        await model.load()
        #expect(model.lastError == nil)
    }

    @Test("An ordinary change writes at once and waits for a restart")
    func ordinary() async throws {
        let api = FakeConfig()
        let model = await loaded(api)
        await model.changeText(parallel, text: "5")
        let write = try #require(api.writes.first)
        #expect(write.values == [parallel.key: "5"])
        #expect(!write.confirmed)
        #expect(model.pendingRestartCount == 1)
        #expect(model.state?.restartRequired == true)
        #expect(model.pending.isEmpty)
    }

    @Test("The same value writes nothing")
    func unchanged() async {
        let api = FakeConfig()
        let model = await loaded(api)
        await model.changeText(parallel, text: "1")
        await model.resetToDefault(parallel)
        #expect(api.writes.isEmpty)
    }

    @Test("Past the limit stays with the field and never reaches orchd")
    func outOfRange() async {
        let api = FakeConfig()
        let model = await loaded(api)
        await model.changeText(parallel, text: "51")
        #expect(api.writes.isEmpty)
        #expect(model.fieldErrors[parallel.key]?.contains("from 0 to 50") == true)
        await model.changeText(parallel, text: "2")
        #expect(model.fieldErrors[parallel.key] == nil)
    }

    @Test("orchd's refusal stays with the field")
    func refusal() async {
        let api = FakeConfig()
        let model = await loaded(api)
        api.fail = Refused()
        await model.change(parallel, json: "5")
        #expect(model.fieldErrors[parallel.key] == "queue.worker.max_concurrent: must be from 0 to 50")
        #expect(model.pending.isEmpty)
    }

    @Test("A sensitive change asks first, and only then says it was confirmed")
    func sensitive() async throws {
        let api = FakeConfig()
        let model = await loaded(api)
        await model.changeText(mode, text: "plan")
        #expect(api.writes.isEmpty)
        #expect(model.confirmation?.json == "\"plan\"")
        model.cancelChange()
        #expect(model.confirmation == nil)
        await model.confirmChange()
        #expect(api.writes.isEmpty)

        await model.changeText(mode, text: "plan")
        await model.confirmChange()
        let write = try #require(api.writes.first)
        #expect(write.confirmed)
        #expect(model.confirmation == nil)
    }

    @Test("Restart runs the daemon's restart, then reads what it runs")
    func restart() async {
        let api = FakeConfig()
        let model = await loaded(api)
        let before = api.loads
        await model.restart()
        #expect(api.loads == before + 1)
        #expect(!model.isRestarting)
        #expect(model.restartError == nil)
    }

    @Test("A failed restart says why and reads nothing")
    func restartFailure() async {
        let api = FakeConfig()
        let model = await loaded(api) { throw Refused() }
        let before = api.loads
        await model.restart()
        #expect(model.restartError?.contains("from 0 to 50") == true)
        #expect(api.loads == before)
    }

    @Test("A daemon slow to come back is retried, then reported")
    func restartSlow() async {
        let api = FakeConfig()
        let model = await loaded(api)
        api.fail = Refused()
        let before = api.loads
        await model.restart()
        #expect(api.loads == before + 10)
        #expect(model.lastError != nil)
    }
}

private final class FakeOwners: DocsOwnersAPI, @unchecked Sendable {
    private let lock = NSLock()
    var owners: [DocsOwner] = []
    var fail: (any Error)?
    var purged: [String] = []
    var removed: [(String, Bool)] = []

    private func check() throws { if let fail { throw fail } }

    func docsOwners() async throws -> [DocsOwner] { try lock.withLock { try check(); return owners } }
    func docsOwnerRepos(owner: String) async throws -> [DocsOwnerRepoChoice] {
        try lock.withLock { try check(); return [DocsOwnerRepoChoice(name: "a", isFork: true)] }
    }
    func saveDocsOwner(owner: String, enabled: Bool, mode: DocsRepoMode, repos: [String]) async throws -> DocsOwner {
        try lock.withLock {
            try check()
            let saved = DocsOwner(owner: owner, enabled: enabled, repoMode: mode, selectedRepos: repos)
            owners.removeAll { $0.owner == owner }
            owners.append(saved)
            return saved
        }
    }
    func removeDocsOwner(owner: String, purge: Bool) async throws -> DocsPurgeResult {
        try lock.withLock {
            try check()
            removed.append((owner, purge))
            owners.removeAll { $0.owner == owner }
            return DocsPurgeResult(sources: purge ? 4 : 0, chunks: purge ? 9 : 0)
        }
    }
    func purgeDocs(owner: String, repos: [String]) async throws -> DocsPurgeResult {
        try lock.withLock {
            try check()
            purged.append(owner + ":" + repos.joined(separator: ","))
            return DocsPurgeResult(sources: 2, chunks: 5)
        }
    }
}

@MainActor
private func ownersModel(_ api: FakeOwners, known: [String] = ["me", "acme"]) async -> DocsOwnersModel {
    let model = DocsOwnersModel(api: api, knownOwners: { known })
    await model.load()
    return model
}

@Suite("DocsOwnersModel")
@MainActor
struct DocsOwnersModelTests {
    @Test("Rows are known accounts then other owners, once each, whatever the case")
    func rows() async {
        let api = FakeOwners()
        api.owners = [DocsOwner(owner: "Acme"), DocsOwner(owner: "public-org", enabled: false)]
        let model = await ownersModel(api)
        #expect(model.rows.map(\.login) == ["me", "Acme", "public-org"])
        #expect(model.rows.map(\.isOn) == [false, true, false])
        #expect(model.rows[0].owner == nil)
    }

    @Test("Registered owners still show when GitHub cannot be reached")
    func withoutGitHub() async {
        struct Offline: Error {}
        let api = FakeOwners()
        api.owners = [DocsOwner(owner: "acme")]
        let model = DocsOwnersModel(api: api, knownOwners: { throw Offline() })
        await model.load()
        #expect(model.rows.map(\.login) == ["acme"])
        #expect(model.lastError == nil)
    }

    @Test("A failed load says why")
    func loadFailure() async {
        let api = FakeOwners()
        api.fail = Refused()
        let model = await ownersModel(api)
        #expect(model.lastError != nil)
    }

    @Test("Opting in and out keeps the repo selection")
    func toggle() async throws {
        let api = FakeOwners()
        let model = await ownersModel(api)
        await model.setEnabled("acme", true)
        #expect(model.rows.first { $0.login == "acme" }?.isOn == true)
        #expect(await model.setRepos("acme", mode: .include, repos: ["b", "a"]))
        await model.setEnabled("acme", false)
        let owner = try #require(api.owners.first)
        #expect(!owner.enabled)
        #expect(owner.repoMode == .include)
        #expect(owner.selectedRepos == ["a", "b"])
        #expect(await model.setRepos("acme", mode: .all, repos: ["ignored"]))
        #expect(api.owners.first?.selectedRepos == [])
    }

    @Test("Adding checks the name first", arguments: [("acme-co", true), (" acme ", true), ("../etc", false), ("a b", false), ("", false)])
    func add(login: String, ok: Bool) async {
        let api = FakeOwners()
        let model = await ownersModel(api, known: [])
        #expect(await model.add(login) == ok)
        #expect(api.owners.count == (ok ? 1 : 0))
        #expect((model.message != nil) == !ok)
    }

    @Test("Removing keeps docs unless asked, and says which it did")
    func remove() async throws {
        let api = FakeOwners()
        api.owners = [DocsOwner(owner: "a"), DocsOwner(owner: "b")]
        let model = await ownersModel(api, known: [])
        await model.remove("a", purge: false)
        #expect(model.message?.contains("stay in memory") == true)
        await model.remove("b", purge: true)
        #expect(model.message?.contains("deleted 4 docs") == true)
        #expect(api.removed.map(\.1) == [false, true])
    }

    @Test("Deleting kept docs reports how many")
    func purge() async {
        let api = FakeOwners()
        let model = await ownersModel(api)
        await model.purgeKept("acme", repos: [])
        #expect(api.purged.isEmpty, "no repos must not reach orchd, which reads it as all of them")
        await model.purgeKept("acme", repos: ["a", "b"])
        #expect(api.purged == ["acme:a,b"])
        #expect(model.message?.contains("Deleted 2 docs") == true)
    }

    @Test("A refusal stays with its owner and clears on the next try")
    func refusal() async {
        let api = FakeOwners()
        let model = await ownersModel(api)
        api.fail = Refused()
        await model.setEnabled("acme", true)
        #expect(model.ownerErrors["acme"] != nil)
        #expect(await model.setRepos("acme", mode: .all, repos: []) == false)
        #expect(model.pending.isEmpty)
        api.fail = nil
        await model.setEnabled("acme", true)
        #expect(model.ownerErrors["acme"] == nil)
    }

    @Test("Repo lists load per owner and report failures")
    func repos() async {
        let api = FakeOwners()
        let model = await ownersModel(api)
        await model.loadRepos("acme")
        #expect(model.repos("acme")?.first?.isFork == true)
        #expect(model.repos("other") == nil)
        api.fail = Refused()
        await model.loadRepos("other")
        #expect(model.repoError("other") != nil)
        #expect(model.repoError("acme") == nil)
    }

    @Test("What an owner covers, in words", arguments: [
        (DocsOwner(owner: "a"), "Every repo"),
        (DocsOwner(owner: "a", enabled: false), "Off"),
        (DocsOwner(owner: "a", repoMode: .include, selectedRepos: ["x"]), "1 repo"),
        (DocsOwner(owner: "a", repoMode: .include, selectedRepos: ["x", "y"]), "2 repos"),
        (DocsOwner(owner: "a", repoMode: .exclude, selectedRepos: ["x"]), "All but 1 repo"),
        (DocsOwner(owner: "a", repoMode: .exclude, selectedRepos: ["x", "y"]), "All but 2 repos"),
    ])
    func coverage(owner: DocsOwner, want: String) {
        #expect(owner.coverage == want)
    }

    @Test("Only opted-out repos with docs count as kept")
    func kept() {
        let owner = DocsOwner(owner: "a", repos: [
            DocsOwnerRepoState(repo: "kept", files: 3, status: "excluded"),
            DocsOwnerRepoState(repo: "empty", files: 0, status: "excluded"),
            DocsOwnerRepoState(repo: "live", files: 3, status: "ok"),
        ])
        #expect(owner.keptRepos.map(\.repo) == ["kept"])
        #expect(DocsRepoMode.allCases.map(\.label).count == 3)
    }
}

@Suite("Settings RPCs")
struct SettingsRPCTests {
    @Test("Config travels both ways over gRPC")
    func config() async throws {
        var service = FakeOrchdService()
        var field = Orchd_V1_ConfigField()
        field.key = "queue.worker.max_concurrent"
        field.section = "Tasks"
        field.label = "Parallel tasks"
        field.type = "int"
        field.value = "3"
        field.defaultValue = "1"
        field.hasRange_p = true
        field.min = 0
        field.max = 50
        field.pendingRestart = true
        var unknownType = Orchd_V1_ConfigField()
        unknownType.key = "future"
        unknownType.type = "from-a-newer-orchd"
        service.configState.fields = [field, unknownType]
        service.configState.path = "/h/config.yaml"
        service.configState.restartRequired = true
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let state = try await client.config()
            #expect(state.path == "/h/config.yaml")
            #expect(state.restartRequired)
            #expect(state.fields[0].range == 0...50)
            #expect(state.fields[0].pendingRestart)
            #expect(state.fields[1].type == .string)
            #expect(state.fields[1].range == nil)
            _ = try await client.setConfig([field.key: "5"], confirmSensitive: true)
            let sent = try #require(service.recorder.last(Orchd_V1_SetConfigRequest.self))
            #expect(sent.values == [field.key: "5"])
            #expect(sent.confirmSensitive)
        }
    }

    @Test("Docs owners travel both ways over gRPC")
    func owners() async throws {
        var service = FakeOrchdService()
        var owner = Orchd_V1_DocOwner()
        owner.owner = "acme"
        owner.enabled = true
        owner.repoMode = "exclude"
        owner.selectedRepos = ["x"]
        owner.lastSynced = 100
        var repo = Orchd_V1_DocRepo()
        repo.repo = "x"
        repo.files = 3
        repo.status = "excluded"
        owner.repos = [repo]
        service.docsOwners.owners = [owner]
        service.docOwner = owner
        var choice = Orchd_V1_DocsOwnerRepo()
        choice.name = "x"
        choice.archived = true
        service.docsOwnerRepos.repos = [choice]
        service.purge.purgedSources = 2
        service.purge.purgedChunks = 7
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let listed = try #require(try await client.docsOwners().first)
            #expect(listed.repoMode == .exclude)
            #expect(listed.lastSynced == Date(timeIntervalSince1970: 100))
            #expect(listed.keptRepos.map(\.repo) == ["x"])

            #expect(try await client.docsOwnerRepos(owner: "acme").first?.isArchived == true)
            #expect(service.recorder.last(Orchd_V1_ListDocsOwnerReposRequest.self)?.owner == "acme")

            _ = try await client.saveDocsOwner(owner: "acme", enabled: true, mode: .all, repos: ["dropped"])
            let saved = try #require(service.recorder.last(Orchd_V1_SaveDocsOwnerRequest.self))
            #expect(saved.repoMode == "all")
            #expect(saved.repos.isEmpty)

            #expect(try await client.removeDocsOwner(owner: "acme", purge: true) == DocsPurgeResult(sources: 2, chunks: 7))
            let removed = try #require(service.recorder.last(Orchd_V1_RemoveDocsOwnerRequest.self))
            #expect(removed.purge && removed.confirm)
            _ = try await client.removeDocsOwner(owner: "acme", purge: false)
            let kept = try #require(service.recorder.last(Orchd_V1_RemoveDocsOwnerRequest.self))
            #expect(!kept.purge && !kept.confirm)

            #expect(try await client.purgeDocs(owner: "acme", repos: ["x"]).chunks == 7)
            let purged = try #require(service.recorder.last(Orchd_V1_PurgeDocsRequest.self))
            #expect(purged.confirm)
            #expect(purged.repos == ["x"])
        }
    }
}
