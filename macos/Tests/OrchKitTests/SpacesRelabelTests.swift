import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

/// Answers Relabel like orchd: a count without apply, a run with it. A run
/// can be held until the test releases it, to look at the model mid-run.
private final class FakeRelabel: SpacesRelabelAPI, @unchecked Sendable {
    private let lock = NSLock()
    /// Counts returned in turn; the last one repeats.
    var counts: [Result<RelabelRun, any Error>] = [.success(RelabelRun(revision: 7, total: 9000, stale: 120))]
    var run: Result<RelabelRun, any Error> = .success(RelabelRun(revision: 7, total: 9000, stale: 120, changed: 120,
                                                                  moved: 30, applied: true))
    /// Hold applied runs until `release()`.
    var holdRuns = false
    /// Hold counts until `release()`.
    var holdCounts = false
    private(set) var calls: [Bool] = []
    private var waiting: [CheckedContinuation<Void, Never>] = []

    var applyCalls: Int { lock.withLock { calls.filter { $0 }.count } }
    var countCalls: Int { lock.withLock { calls.filter { !$0 }.count } }

    func relabel(apply: Bool) async throws -> RelabelRun {
        let hold = lock.withLock {
            calls.append(apply)
            return apply ? holdRuns : holdCounts
        }
        if hold { await withCheckedContinuation { c in lock.withLock { waiting.append(c) } } }
        return try lock.withLock {
            if apply { return try run.get() }
            let next = counts.count > 1 ? counts.removeFirst() : counts[0]
            return try next.get()
        }
    }

    var held: Int { lock.withLock { waiting.count } }

    func release() {
        let all = lock.withLock {
            let w = waiting
            waiting = []
            return w
        }
        all.forEach { $0.resume() }
    }
}

private struct Down: Error, CustomStringConvertible {
    var description: String { "orchd is not running" }
}

/// An Overview with one space, so a relabel's badge can show.
private struct OneSpace: SpacesOverviewAPI {
    func enforcement() async throws -> EnforcementState { EnforcementState() }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }

    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
    func listSpaces() async throws -> SpacesListing {
        SpacesListing(status: SpacesPolicyStatus(configured: true, appliedRevision: 7),
                      spaces: [SpaceInfo(name: "acme", kind: "isolated")])
    }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] { [] }
    func toolLogSummary() async throws -> [SpaceActivity] { [] }
    func unconfirmedGitHubOrgs() async throws -> [String] { [] }
}

@MainActor
@Suite struct SpacesRelabelModelTests {
    /// SP-079.1 a1: stale chunks show the control; pressing it runs Relabel
    /// with apply, its progress shows in the Overview and the sidebar badge
    /// until the run ends, and a count afterwards finds nothing stale.
    @Test func relabelNowRunsWhileChunksAreStaleAndShowsProgressUntilNoneRemain() async throws {
        let api = FakeRelabel()
        api.counts = [.success(RelabelRun(revision: 7, total: 9000, stale: 120)),
                      .success(RelabelRun(revision: 7, total: 9000, stale: 0))]
        api.holdRuns = true
        let model = SpacesRelabelModel(api: api)
        let overview = SpacesOverviewModel(api: OneSpace(), defaults: freshDefaults(), relabel: { await model.progress })
        var changes = 0
        model.onChange = {
            changes += 1
            await overview.load()
        }
        #expect(!model.isVisible, "nothing shows before the first count")

        await model.check()
        #expect(model.phase == .stale(120))
        #expect(model.revision == 7)
        #expect(model.isVisible && model.canStart)
        #expect(api.applyCalls == 0, "a count changes nothing")

        let run = Task { await model.start() }
        try await eventually { api.held == 1 }
        #expect(model.phase == .running(120))
        #expect(model.isRunning && !model.canStart && model.isVisible)
        #expect(model.progress == RelabelProgress(done: 0, total: 120))
        #expect(overview.badge == .relabel(RelabelProgress(done: 0, total: 120)))
        #expect(overview.overview?.needsYou.map(\.title) == ["Relabelling stored content: 120 chunks"])
        #expect(overview.badge.label == "Relabelling: 120 chunks", "no percentage orchd never reported")
        // A second press and a count while it runs do nothing: orchd holds
        // its lock for the run.
        await model.start()
        await model.check()
        #expect(api.applyCalls == 1)
        #expect(api.countCalls == 1)

        api.release()
        await run.value
        #expect(model.phase == .upToDate)
        #expect(model.progress == nil)
        #expect(model.failure == nil)
        #expect(model.lastRun == RelabelRun(revision: 7, total: 9000, stale: 120, changed: 120, moved: 30, applied: true))
        #expect(model.isVisible, "the run's result stays on show")
        #expect(!model.canStart)
        #expect(overview.badge == .none)
        #expect(changes == 2)
        #expect(api.applyCalls == 1)
    }

    /// SP-079.1 a2: with nothing stale the control is absent and pressing
    /// it does nothing.
    @Test func nothingStaleHidesTheControl() async {
        let api = FakeRelabel()
        api.counts = [.success(RelabelRun(revision: 7, total: 9000, stale: 0))]
        let model = SpacesRelabelModel(api: api)
        await model.check()
        #expect(model.phase == .upToDate)
        #expect(!model.isVisible)
        #expect(!model.canStart)
        await model.start()
        #expect(api.applyCalls == 0)
        #expect(model.progress == nil)
    }

    /// SP-079.1 a2: a refused relabel shows orchd's reason and is not tried
    /// again on its own.
    @Test func refusedRelabelShowsWhyWithoutRetrying() async throws {
        let api = FakeRelabel()
        api.run = .failure(RPCError(code: .permissionDenied,
                                    message: "relabel the RAG store is operator-only under a spaces policy"))
        let model = SpacesRelabelModel(api: api)
        await model.check()
        await model.start()
        #expect(model.failure == "relabel the RAG store is operator-only under a spaces policy")
        #expect(model.phase == .stale(120), "counted again; still stale")
        #expect(model.canStart, "the user may press it again")
        #expect(model.isVisible)
        #expect(model.lastRun == nil)
        try await Task.sleep(for: .milliseconds(100))
        #expect(api.applyCalls == 1)
        #expect(api.countCalls == 2)
    }

    /// SP-079.1 a2: a run that stops early, or leaves chunks stale, says so.
    @Test func failedRunShowsWhyWithoutRetrying() async throws {
        let api = FakeRelabel()
        api.run = .success(RelabelRun(revision: 7, stale: 120, changed: 20, failed: 100, applied: true,
                                      error: "the applied policy changed during the relabel (revision 8); run it again"))
        api.counts = [.success(RelabelRun(revision: 7, stale: 120)), .success(RelabelRun(revision: 7, stale: 100))]
        let model = SpacesRelabelModel(api: api)
        await model.check()
        await model.start()
        #expect(model.failure == "the applied policy changed during the relabel (revision 8); run it again")
        #expect(model.phase == .stale(100))
        #expect(model.lastRun?.changed == 20)

        api.run = .success(RelabelRun(revision: 7, stale: 100, changed: 99, failed: 1, applied: true))
        api.counts = [.success(RelabelRun(revision: 7, stale: 1))]
        await model.start()
        #expect(model.failure == "1 chunk could not be placed and is still stale. Relabel again later; orchd's log says why.")
        #expect(model.phase == .stale(1))

        api.run = .success(RelabelRun(revision: 7, stale: 3, changed: 0, failed: 3, applied: true))
        api.counts = [.success(RelabelRun(revision: 7, stale: 3))]
        await model.start()
        #expect(model.failure == "3 chunks could not be placed and are still stale. Relabel again later; orchd's log says why.")
        try await Task.sleep(for: .milliseconds(100))
        #expect(api.applyCalls == 3, "one run per press")

        // A clean run clears the reason.
        api.run = .success(RelabelRun(revision: 7, stale: 3, changed: 3, applied: true))
        api.counts = [.success(RelabelRun(revision: 7, stale: 0))]
        await model.start()
        #expect(model.failure == nil)
        #expect(model.phase == .upToDate)
    }

    /// A count that fails says why and offers to count again; it is not
    /// repeated on its own either.
    @Test func failedCountShowsWhy() async throws {
        let api = FakeRelabel()
        api.counts = [.failure(RPCError(code: .failedPrecondition,
                                        message: "a hybrid migration, backup drop, keyword re-index or relabel is already running")),
                      .success(RelabelRun(revision: 7, stale: 5))]
        let model = SpacesRelabelModel(api: api)
        await model.check()
        #expect(model.phase == .unknown)
        #expect(model.checkError == "a hybrid migration, backup drop, keyword re-index or relabel is already running")
        #expect(model.isVisible && !model.canStart)
        await model.start()
        #expect(api.applyCalls == 0)
        try await Task.sleep(for: .milliseconds(50))
        #expect(api.countCalls == 1)
        await model.check()
        #expect(model.phase == .stale(5))
        #expect(model.checkError == nil)

        let down = FakeRelabel()
        down.counts = [.failure(Down())]
        let other = SpacesRelabelModel(api: down)
        await other.check()
        #expect(other.checkError?.isEmpty == false)
    }

    /// A run that ends with its follow-up count failing still shows the
    /// run and why the count failed.
    @Test func runWhoseRecountFailsStillReports() async {
        let api = FakeRelabel()
        api.counts = [.success(RelabelRun(revision: 7, stale: 2)), .failure(Down())]
        let model = SpacesRelabelModel(api: api)
        await model.check()
        await model.start()
        #expect(model.lastRun?.changed == 120)
        #expect(model.phase == .unknown)
        #expect(model.checkError != nil)
        #expect(model.isVisible)
    }

    /// The button cannot start a second run before the first run's follow-up
    /// count answers.
    @Test func noSecondRunBeforeTheRecountAnswers() async throws {
        let api = FakeRelabel()
        api.counts = [.success(RelabelRun(revision: 7, stale: 120)), .success(RelabelRun(revision: 7, stale: 0))]
        let model = SpacesRelabelModel(api: api)
        await model.check()
        api.holdCounts = true
        let run = Task { await model.start() }
        try await eventually { api.held == 1 }
        #expect(api.applyCalls == 1)
        #expect(model.isChecking && !model.canStart)
        await model.start()
        #expect(api.applyCalls == 1, "a press while the recount is out does nothing")
        api.release()
        await run.value
        #expect(model.phase == .upToDate)
        #expect(api.applyCalls == 1)
    }

    /// Leaving the pane cancels its count, which is not shown as a failure.
    @Test func cancelledCountIsNotAFailure() async throws {
        let api = FakeRelabel()
        api.holdCounts = true
        api.counts = [.failure(CancellationError())]
        let model = SpacesRelabelModel(api: api)
        let count = Task { await model.check() }
        try await eventually { api.held == 1 }
        count.cancel()
        api.release()
        await count.value
        #expect(model.checkError == nil)
        #expect(model.phase == .unknown)
        #expect(!model.isVisible)
        #expect(!model.isChecking)
    }

    /// A run with nothing known done says how many chunks it relabels, not
    /// "0%"; a reported share still shows as a percentage.
    @Test func progressWithoutAShareNamesTheChunks() {
        #expect(RelabelProgress(done: 0, total: 120).summary == "120 chunks")
        #expect(RelabelProgress(done: 0, total: 1).summary == "1 chunk")
        #expect(RelabelProgress(done: 30, total: 120).summary == "25%")
    }

    /// orchd's text is made safe to show.
    @Test func failureTextIsDisplaySafe() async {
        let api = FakeRelabel()
        api.run = .success(RelabelRun(revision: 7, stale: 1, applied: true, error: "bad\u{202E}txt"))
        let model = SpacesRelabelModel(api: api)
        await model.check()
        await model.start()
        #expect(model.failure?.contains("\u{202E}") == false)
    }
}

@MainActor
private func freshDefaults() -> UserDefaults {
    let name = "orch-spaces-relabel-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

@Suite struct SpacesRelabelClientTests {
    /// The client sends apply as asked and reads every count back.
    @Test func daemonClientCallsRelabel() async throws {
        var response = Orchd_V1_RelabelResponse()
        response.revision = 7
        response.total = 9000
        response.stale = 120
        response.changed = 118
        response.moved = 30
        response.failed = 2
        response.applied = true
        response.error = "stopped"
        var transition = Orchd_V1_RelabelTransition()
        transition.from = "unplaced"
        transition.to = "acme"
        transition.chunks = 118
        response.transitions = [transition]
        let service = FakeRelabelService(response: response)
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let got = try await client.relabel(apply: true)
            #expect(service.recorder.last(Orchd_V1_RelabelRequest.self)?.apply == true)
            #expect(got == RelabelRun(revision: 7, total: 9000, stale: 120, changed: 118, moved: 30, failed: 2,
                                      applied: true, error: "stopped"))
            _ = try await client.relabel(apply: false)
            #expect(service.recorder.last(Orchd_V1_RelabelRequest.self)?.apply == false)
        }
    }

    /// orchd's refusal reaches the model as its message.
    @Test func refusalReachesTheModel() async throws {
        let service = FakeRelabelService(refusal: RPCError(code: .permissionDenied, message: "operator only"))
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let model = await SpacesRelabelModel(api: client)
            await model.check()
            #expect(await model.checkError == "operator only")
        }
    }
}

/// Relabel served like orchd.
private struct FakeRelabelService: RegistrableRPCService {
    var response = Orchd_V1_RelabelResponse()
    var refusal: RPCError?
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let response = response, refusal = refusal, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Relabel.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RelabelRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RelabelResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            if let refusal { return StreamingServerResponse(single: ServerResponse(error: refusal)) }
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
    }
}
