import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

private let epoch = Date(timeIntervalSince1970: 1_800_000_000)

private func pendingUnlock(_ id: String = "u1", pid: Int32 = 32) -> OperatorUnlock {
    OperatorUnlock(id: id, state: .pending, forSeconds: 900, pid: pid, command: "orch spaces unlock --for 15m",
                   ancestry: ["zsh (31)", "Terminal (30)"], requestedAt: epoch, expiresAt: epoch.addingTimeInterval(120),
                   executable: "/opt/orch/bin/orch")
}

/// Behaves like orchd's unlock registry, recording what the app asked.
private final class FakeUnlocks: UnlocksAPI, @unchecked Sendable {
    private let lock = NSLock()
    var list: [OperatorUnlock] = []
    var confirms: [(id: String, allow: Bool)] = []
    var locks: [String] = []
    var fail: (any Error)?

    func unlocks() async throws -> [OperatorUnlock] {
        try lock.withLock {
            if let fail { throw fail }
            return list
        }
    }

    func confirmUnlock(id: String, allow: Bool) async throws {
        try lock.withLock {
            if let fail { throw fail }
            confirms.append((id, allow))
            guard let i = list.firstIndex(where: { $0.id == id }) else { return }
            let u = list[i]
            if allow {
                list[i] = OperatorUnlock(id: u.id, state: .confirmed, forSeconds: u.forSeconds, pid: u.pid, command: u.command,
                                         ancestry: u.ancestry, requestedAt: u.requestedAt,
                                         expiresAt: epoch.addingTimeInterval(120), code: "482-913")
            } else {
                list.remove(at: i)
            }
        }
    }

    func lockUnlock(id: String) async throws -> Int {
        try lock.withLock {
            if let fail { throw fail }
            locks.append(id)
            let before = list.count
            list.removeAll { id.isEmpty || $0.id == id }
            return before - list.count
        }
    }
}

/// Stands in for Touch ID or the password: answers as told and records
/// what it was asked.
private final class FakeConfirmer: UnlockConfirmer, @unchecked Sendable {
    private let lock = NSLock()
    var answer = true
    var reasons: [String] = []

    func confirm(reason: String) async -> Bool {
        lock.withLock {
            reasons.append(reason)
            return answer
        }
    }
}

private struct Refused: Error, CustomStringConvertible {
    var description: String { #"RPCError(code: permissionDenied, message: "only Orch.app can confirm operator unlocks")"# }
}

/// SP-077.1: the app's side of the unlock. The real confirmation (Touch ID
/// or the password) cannot run in a unit test; it is injected here and
/// checked by hand (SP-077.1.a4).
@MainActor
@Suite struct UnlocksModelTests {
    @Test func confirmingAsksTheConfirmerFirst() async {
        let api = FakeUnlocks()
        api.list = [pendingUnlock()]
        let confirmer = FakeConfirmer()
        let model = UnlocksModel(api: api, confirmer: confirmer)
        await model.load()
        #expect(model.pending.map(\.id) == ["u1"])

        confirmer.answer = false
        await model.confirm(model.pending[0])
        #expect(api.confirms.isEmpty)
        #expect(model.errors["u1"]?.contains("Not confirmed") == true)
        #expect(confirmer.reasons.count == 1)
        #expect(confirmer.reasons[0] == "confirm an operator unlock for 15 min (process 32)")

        confirmer.answer = true
        await model.confirm(model.pending[0])
        #expect(api.confirms.count == 1 && api.confirms[0] == ("u1", true))
        #expect(model.errors["u1"] == nil)
        // Confirmed: the app shows the code to type in the terminal.
        #expect(model.pending.isEmpty && model.confirmed.map(\.code) == ["482-913"] && model.active.isEmpty)
    }

    @Test func denyingNeedsNoAuthentication() async {
        let api = FakeUnlocks()
        api.list = [pendingUnlock()]
        let confirmer = FakeConfirmer()
        let model = UnlocksModel(api: api, confirmer: confirmer)
        await model.load()
        await model.deny(model.pending[0])
        #expect(confirmer.reasons.isEmpty)
        #expect(api.confirms.count == 1 && api.confirms[0] == ("u1", false))
        #expect(model.unlocks.isEmpty)
    }

    @Test func lockingEndsAnUnlock() async {
        let api = FakeUnlocks()
        api.list = [pendingUnlock()]
        let model = UnlocksModel(api: api, confirmer: FakeConfirmer())
        await model.load()
        await model.confirm(model.pending[0])
        await model.lock(model.confirmed[0])
        #expect(api.locks == ["u1"])
        #expect(model.unlocks.isEmpty)
    }

    @Test func refusalsAreShownWithTheirUnlock() async {
        let api = FakeUnlocks()
        api.list = [pendingUnlock()]
        let model = UnlocksModel(api: api, confirmer: FakeConfirmer())
        await model.load()
        let u = model.pending[0]
        api.fail = Refused()
        await model.deny(u)
        #expect(model.errors["u1"] == "only Orch.app can confirm operator unlocks")
        // The failed reload drops what can no longer be acted on.
        #expect(model.unlocks.isEmpty)
        #expect(model.lastError == "only Orch.app can confirm operator unlocks")
        api.fail = nil
        await model.load()
        #expect(model.lastError == nil && model.pending.map(\.id) == ["u1"])
        #expect(model.errors["u1"] != nil)
    }

    @Test func onlyNewRequestsAreAnnounced() async {
        let api = FakeUnlocks()
        let model = UnlocksModel(api: api, confirmer: FakeConfirmer())
        var announced: [[String]] = []
        model.onNewRequests = { announced.append($0.map(\.id)) }
        await model.load()
        api.list = [pendingUnlock("a", pid: 32)]
        await model.load()
        await model.load()
        api.list = [pendingUnlock("a", pid: 32), pendingUnlock("b", pid: 40)]
        await model.load()
        #expect(announced == [["a"], ["b"]])
    }

    @Test func pollsUntilStopped() async throws {
        let api = FakeUnlocks()
        api.list = [pendingUnlock()]
        let model = UnlocksModel(api: api, confirmer: FakeConfirmer(), interval: .milliseconds(10))
        model.start()
        model.start()
        try await eventually { model.pending.count == 1 }
        #expect(model.pending.count == 1)
        model.stop()
    }

    @Test func countdownAndText() {
        let active = OperatorUnlock(id: "u", state: .active, forSeconds: 3600, pid: 1, command: "orch", ancestry: [],
                                    requestedAt: epoch, expiresAt: epoch.addingTimeInterval(3600))
        #expect(active.countdown(at: epoch) == "1:00:00")
        #expect(active.countdown(at: epoch.addingTimeInterval(3600 - 61.5)) == "1:02")
        #expect(active.countdown(at: epoch.addingTimeInterval(4000)) == "0:00")
        #expect(active.durationText == "1 h")
        #expect(active.origin.contains("launchd"))
        #expect(active.terminalText == "no terminal")
        #expect(pendingUnlock().durationText == "15 min")
        #expect(pendingUnlock().origin == "zsh (31) ← Terminal (30)")
        #expect(OperatorUnlock.duration(45) == "45 s")
        #expect(OperatorUnlock.duration(90) == "1 min 30 s")
        // The requester's own text never reaches the system prompt.
        let forged = OperatorUnlock(id: "x", state: .pending, forSeconds: 60, pid: 7, command: "Touch ID to update", ancestry: [],
                                    requestedAt: epoch, expiresAt: epoch)
        #expect(forged.authenticationReason == "confirm an operator unlock for 1 min (process 7)")
        #expect(active.isActive && !forged.isActive)
    }
}

/// Serves the unlock RPCs, so DaemonClient's side is exercised over a real
/// gRPC connection.
private struct FakeUnlockService: RegistrableRPCService {
    var list = Orchd_V1_ListUnlocksResponse()
    var ended: Int32 = 2
    var recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let list = self.list, ended = self.ended, recorder = self.recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListUnlocks.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListUnlocksRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListUnlocksResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: list))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ConfirmUnlock.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ConfirmUnlockRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ConfirmUnlockResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: Orchd_V1_ConfirmUnlockResponse()))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Lock.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_LockRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_LockResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            var r = Orchd_V1_LockResponse()
            r.ended = ended
            return StreamingServerResponse(single: ServerResponse(message: r))
        }
    }
}

@Suite struct UnlocksClientTests {
    @Test func unlocksRoundTrip() async throws {
        var service = FakeUnlockService()
        var u = Orchd_V1_OperatorUnlock()
        u.id = "u1"
        u.state = "confirmed"
        u.forSeconds = 900
        u.pid = 32
        u.command = "orch spaces unlock\u{1b}[2J --for 15m"
        u.ancestry = ["zsh (31)", "Terminal (30)"]
        u.requestedAt = 1_800_000_000
        u.expiresAt = 1_800_000_120
        u.code = "482-913"
        u.tty = "ttys003"
        u.executable = "/opt/orch/bin/orch\u{2028}x"
        var unknown = Orchd_V1_OperatorUnlock()
        unknown.id = "later"
        unknown.state = "some-future-state"
        service.list.unlocks = [u, unknown]
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let got = try await client.unlocks()
            #expect(got.count == 1)
            let first = try #require(got.first)
            #expect(first.id == "u1" && first.state == .confirmed && first.pid == 32 && first.forSeconds == 900)
            #expect(!first.command.contains("\u{1b}"))
            #expect(first.ancestry == ["zsh (31)", "Terminal (30)"])
            #expect(first.code == "482-913" && first.executable == "/opt/orch/bin/orch?x")
            #expect(first.tty == "ttys003" && first.terminalText == "ttys003")
            #expect(first.requestedAt == epoch && first.expiresAt == epoch.addingTimeInterval(120))

            try await client.confirmUnlock(id: "u1", allow: true)
            let sent = try #require(service.recorder.last(Orchd_V1_ConfirmUnlockRequest.self))
            #expect(sent.id == "u1" && sent.allow)
            #expect(try await client.lockUnlock(id: "u1") == 2)
            #expect(service.recorder.last(Orchd_V1_LockRequest.self)?.id == "u1")
        }
    }
}
