import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private let epoch = Date(timeIntervalSince1970: 1_800_000_000)

private func layers() -> [EnforcementSwitch] {
    [
        EnforcementSwitch(layer: "memory", title: "memory filtering", state: "on", defaultState: "on", states: ["on", "off"]),
        EnforcementSwitch(layer: "guard", title: "tool guard", state: "block-other-jobs", defaultState: "block-other-jobs",
                          states: ["off", "log", "block-other-jobs", "strict"]),
        EnforcementSwitch(layer: "terms", title: "outbound terms", state: "on", defaultState: "on", states: ["on", "off"]),
        EnforcementSwitch(layer: "sandbox", title: "sandbox rules", state: "on", defaultState: "on", states: ["on", "off"]),
    ]
}

private struct Refused: Error, CustomStringConvertible {
    var description: String { #"RPCError(code: permissionDenied, message: "only the operator can switch enforcement")"# }
}

/// Behaves like orchd's switches: a relaxing state is relaxed, a timed
/// switch records when it reverts and to what.
private final class FakeSpaces: SpacesAPI, @unchecked Sendable {
    private let lock = NSLock()
    var state = EnforcementState(switches: layers())
    var sets: [(layer: String, state: String, seconds: Int64)] = []
    var reads = 0
    var fail: (any Error)?
    var answers: [String: PlacementExplanation] = [:]
    var asked: [String] = []

    func enforcement() async throws -> EnforcementState {
        try lock.withLock {
            reads += 1
            if let fail { throw fail }
            return state
        }
    }

    func setEnforcement(layer: String, state new: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        try lock.withLock {
            if let fail { throw fail }
            sets.append((layer, new, forSeconds))
            let i = state.switches.firstIndex { $0.layer == layer }!
            var s = state.switches[i]
            let before = s.state
            s.state = new
            s.relaxed = s.relaxes(to: new)
            s.setAt = epoch
            s.setBy = "operator (process 9)"
            s.until = forSeconds > 0 ? epoch.addingTimeInterval(TimeInterval(forSeconds)) : nil
            s.revertsTo = forSeconds > 0 ? before : ""
            state.switches[i] = s
            return EnforcementChangeResult(current: s, changed: before != new, notes: ["noted\u{7}"])
        }
    }

    func explainPlacement(target: String) async throws -> PlacementExplanation {
        try lock.withLock {
            asked.append(target)
            if let fail { throw fail }
            return answers[target] ?? PlacementExplanation(rule: "unmatched", outcome: "quarantined", space: "quarantine")
        }
    }
}

@MainActor
@Suite struct EnforcementModelTests {
    @Test func loadsEverySwitchAndTheRelaxedOnes() async {
        let api = FakeSpaces()
        api.state.switches[1].state = "log"
        api.state.switches[1].relaxed = true
        let model = EnforcementModel(api: api)
        #expect(model.relaxed.isEmpty)
        await model.load()
        #expect(model.state?.switches.map(\.layer) == ["memory", "guard", "terms", "sandbox"])
        #expect(model.relaxed.map(\.layer) == ["guard"])
        #expect(model.lastError == nil)
    }

    @Test func aLoadErrorIsShownAndClearedByTheNextRead() async {
        let api = FakeSpaces()
        api.fail = Refused()
        let model = EnforcementModel(api: api)
        await model.load()
        #expect(model.lastError == "only the operator can switch enforcement")
        api.fail = nil
        await model.load()
        #expect(model.lastError == nil)
    }

    @Test func switchingOffAsksFirstAndSendsOnlyOnConfirm() async {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api)
        await model.load()
        let memory = model.state!.switches[0]
        await model.choose(memory, state: "off", duration: .minutes15)
        #expect(api.sets.isEmpty)
        #expect(model.confirmation?.title == "Stop enforcing memory filtering?")
        #expect(model.confirmation?.message.contains("reverts to on after 15 minutes") == true)
        model.cancel()
        #expect(model.confirmation == nil)
        #expect(api.sets.isEmpty)

        await model.choose(memory, state: "off", duration: .minutes15)
        await model.confirm()
        #expect(api.sets.count == 1)
        #expect(api.sets[0] == ("memory", "off", 900))
        #expect(model.relaxed.map(\.layer) == ["memory"])
        #expect(model.state?.switches[0].until == epoch.addingTimeInterval(900))
        // Notes come from orchd and are made safe to show.
        #expect(model.notes["memory"] == ["noted\\u{0007}"])
        await model.confirm()
        #expect(api.sets.count == 1)
    }

    @Test func everyLayerCanBeSwitchedToEachOfItsStates() async {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api)
        await model.load()
        var cases = 0
        for layer in layers() {
            for target in layer.states where target != layer.state {
                await model.choose(model.state!.switches.first { $0.layer == layer.layer }!, state: target)
                if layer.relaxes(to: target) { await model.confirm() }
                let now = model.state!.switches.first { $0.layer == layer.layer }!
                #expect(now.state == target, "\(layer.layer) -> \(target)")
                #expect(now.relaxed == layer.relaxes(to: target))
                cases += 1
                // Back to the default, which never asks.
                await model.choose(now, state: layer.defaultState)
                #expect(model.confirmation == nil)
            }
        }
        #expect(cases == 6)
        #expect(model.relaxed.isEmpty)
    }

    @Test func tighteningIsSentAtOnceAndTheSameStateIsNotSent() async {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api)
        await model.load()
        let guardLayer = model.state!.switches[1]
        await model.choose(guardLayer, state: "block-other-jobs")
        #expect(api.sets.isEmpty)
        await model.choose(guardLayer, state: "strict", duration: .hour1)
        #expect(model.confirmation == nil)
        #expect(api.sets.count == 1)
        #expect(api.sets[0] == ("guard", "strict", 3600))
        // The same state again with a timer is a change: it starts a timer.
        await model.choose(model.state!.switches[1], state: "strict", duration: .minutes15)
        #expect(api.sets.count == 2)
    }

    @Test func aRefusalStaysWithItsLayer() async {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api)
        await model.load()
        api.fail = Refused()
        await model.choose(model.state!.switches[2], state: "on", duration: .hour1)
        #expect(model.layerErrors["terms"] == "only the operator can switch enforcement")
        #expect(model.pending.isEmpty)
        api.fail = nil
        await model.choose(model.state!.switches[2], state: "on", duration: .hour1)
        #expect(model.layerErrors["terms"] == nil)
    }

    @Test func pollsAtTheIntervalOrJustAfterTheNextTimer() async {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api, interval: .seconds(10), now: { epoch })
        #expect(model.nextPoll() == .seconds(10))
        await model.load()
        #expect(model.nextPoll() == .seconds(10))
        api.state.switches[0].until = epoch.addingTimeInterval(3)
        api.state.switches[1].until = epoch.addingTimeInterval(60)
        await model.load()
        #expect(model.nextPoll() == .milliseconds(3500))
        api.state.switches[0].until = epoch.addingTimeInterval(-5)
        await model.load()
        #expect(model.nextPoll() == .milliseconds(500))
    }

    @Test func startReadsAndStopEnds() async throws {
        let api = FakeSpaces()
        let model = EnforcementModel(api: api, interval: .milliseconds(20))
        model.start()
        model.start()
        let deadline = ContinuousClock.now + .seconds(5)
        while api.reads < 2, ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(10)) }
        model.stop()
        #expect(api.reads >= 2)
        #expect(model.state != nil)
    }
}

@Suite struct EnforcementSwitchTests {
    @Test func relaxingStates() {
        let guardLayer = layers()[1]
        #expect(guardLayer.relaxes(to: "off"))
        #expect(guardLayer.relaxes(to: "log"))
        #expect(!guardLayer.relaxes(to: "strict"))
        #expect(!layers()[0].relaxes(to: "log"))
        #expect(layers()[0].relaxes(to: "off"))
    }

    @Test func countdown() {
        var s = layers()[1]
        #expect(s.countdown(at: epoch) == nil)
        #expect(s.remaining(at: epoch) == nil)
        s.until = epoch.addingTimeInterval(899.2)
        s.revertsTo = "block-other-jobs"
        #expect(s.countdown(at: epoch) == "reverts to block-other-jobs in 15:00")
        #expect(s.countdown(at: epoch.addingTimeInterval(1000)) == "reverts to block-other-jobs in 0:00")
        s.revertsTo = ""
        s.until = epoch.addingTimeInterval(3723)
        #expect(s.countdown(at: epoch) == "reverts to its earlier state in 1:02:03")
        #expect(EnforcementSwitch.clock(59) == "0:59")
    }

    @Test func namesAndConfirmationText() {
        var s = layers()[0]
        #expect(s.name == "memory filtering")
        s.title = ""
        #expect(s.name == "memory")
        let tighten = EnforcementChange(layer: layers()[1], state: "strict", duration: .untilChanged)
        #expect(tighten.title == "Switch tool guard to strict?")
        #expect(tighten.message.contains("It stays strict until you change it."))
        // orchd's strings are made safe in the confirmation prompt.
        var odd = layers()[0]
        odd.title = "memory\u{202E}"
        let prompt = EnforcementChange(layer: odd, state: "off\u{7}", duration: .hour1)
        #expect(prompt.title == "Switch memory\\u{202E} to off\\u{0007}?")
        #expect(!prompt.message.contains("\u{202E}") && !prompt.message.contains("\u{7}"))
        #expect(EnforcementDuration.allCases.map(\.seconds) == [0, 900, 3600, 14400, 86400])
        #expect(EnforcementDuration.hours4.label == "For 4 hours")
        #expect(EnforcementDuration.hours24.id == 86400)
    }

    @Test func stateNextExpiry() {
        var state = EnforcementState(switches: layers())
        #expect(state.nextExpiry == nil)
        state.switches[2].until = epoch.addingTimeInterval(30)
        state.switches[3].until = epoch.addingTimeInterval(10)
        #expect(state.nextExpiry == epoch.addingTimeInterval(10))
    }
}

@MainActor
@Suite struct PlacementValidatorModelTests {
    @Test func targetExpandsHomeAndRefusesRelativePaths() throws {
        #expect(try PlacementValidator.target("  ~/code/x \n", home: "/Users/me").get() == "/Users/me/code/x")
        #expect(try PlacementValidator.target("~", home: "/Users/me").get() == "/Users/me")
        #expect(try PlacementValidator.target("/tmp", home: "/Users/me").get() == "/tmp")
        #expect(throws: PlacementInputError(message: "Type or choose a folder.")) { try PlacementValidator.target("  ", home: "/h").get() }
        #expect(throws: PlacementInputError(message: "Use a full path, starting with / or ~.")) {
            try PlacementValidator.target("code/x", home: "/h").get()
        }
    }

    @Test func checkShowsTheAnswerOrTheError() async {
        let api = FakeSpaces()
        api.answers["/h/code"] = PlacementExplanation(roots: [PlacementRoot(root: "/h/code", space: "oss", kind: "public")],
                                                      rule: "root", outcome: "placed", space: "oss")
        let v = PlacementValidator(api: api, home: "/h")
        v.path = "relative"
        await v.check()
        #expect(v.error == "Use a full path, starting with / or ~.")
        #expect(api.asked.isEmpty)

        v.path = "~/code"
        await v.check()
        #expect(v.checked == "/h/code")
        #expect(v.error == nil)
        #expect(v.explanation?.result == "oss")
        #expect(v.chain.map(\.decides) == [true])
        #expect(!v.isChecking)

        // A picked folder is checked exactly, trailing space and all.
        await v.check(picked: "/h/code ")
        #expect(api.asked.last == "/h/code ")
        #expect(v.path == "/h/code " && v.checked == "/h/code ")
        await v.check(picked: "relative")
        #expect(v.error == "Use a full path, starting with / or ~.")

        api.fail = Refused()
        v.path = "~/code"
        await v.check()
        #expect(v.explanation == nil)
        #expect(v.checked == nil)
        #expect(v.error == "only the operator can switch enforcement")
        #expect(PlacementValidator(api: api).chain.isEmpty)
    }

    @Test func resultsAndRemoteAdjustment() {
        let remote = PlacementRemote(owner: "acme", repo: "app", visibility: "private", space: "acme", kind: "isolated", effect: "x")
        #expect(remote.label == "acme/app (private)")
        #expect(PlacementRemote(owner: "a", repo: "b").label == "a/b")
        #expect(PlacementExplanation(rule: "conflict", outcome: "conflict", space: "quarantine").result == "conflict: nothing is indexed")
        #expect(PlacementExplanation(outcome: "quarantined").result == "quarantine")
        let root = [PlacementRoot(root: "/c", space: "oss", kind: "public")]
        #expect(PlacementExplanation(roots: root, remote: remote, rule: "remote_stricter").remoteAdjusted)
        #expect(!PlacementExplanation(roots: root, remote: remote, rule: "root_stricter").remoteAdjusted)
        #expect(!PlacementExplanation(roots: root, rule: "root").remoteAdjusted)
        #expect(PlacementExplanation(remote: remote, rule: "remote").remoteAdjusted)
    }
}

/// SP-072.2.a1: for the nested-root fixture (~/code public, ~/code/willesq
/// private, ~/code/company1 isolated), the validator's chain and result
/// equal ExplainPlacement's. The fixture is ExplainPlacement's own output,
/// kept current by internal/daemon's TestAppPlacementFixtureMatchesExplainPlacement;
/// here it is served over gRPC and read through the app's client and validator.
@Suite struct PlacementValidatorTests {
    private struct Case: Decodable {
        let path: String
        let explanation: AnyJSON
    }

    /// Keeps the explanation's JSON as it is, to hand to SwiftProtobuf.
    private struct AnyJSON: Decodable {
        let json: String
        init(from decoder: any Decoder) throws {
            let value = try decoder.singleValueContainer().decode(JSONValue.self)
            json = String(decoding: try JSONEncoder().encode(value), as: UTF8.self)
        }
    }

    private indirect enum JSONValue: Codable {
        case string(String), number(Double), bool(Bool), array([JSONValue]), object([String: JSONValue]), null

        init(from decoder: any Decoder) throws {
            let c = try decoder.singleValueContainer()
            if c.decodeNil() { self = .null } else if let b = try? c.decode(Bool.self) { self = .bool(b) }
            else if let n = try? c.decode(Double.self) { self = .number(n) }
            else if let s = try? c.decode(String.self) { self = .string(s) }
            else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
            else { self = .object(try c.decode([String: JSONValue].self)) }
        }

        func encode(to encoder: any Encoder) throws {
            var c = encoder.singleValueContainer()
            switch self {
            case .string(let s): try c.encode(s)
            case .number(let n): try c.encode(n)
            case .bool(let b): try c.encode(b)
            case .array(let a): try c.encode(a)
            case .object(let o): try c.encode(o)
            case .null: try c.encodeNil()
            }
        }
    }

    private static let code = "/Users/fixture/code"

    private static func fixture() throws -> [(path: String, answer: Orchd_V1_PlacementExplanation)] {
        let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Fixtures/placement-explanations.json")
        let cases = try JSONDecoder().decode([Case].self, from: Data(contentsOf: url))
        return try cases.map {
            let json = $0.explanation.json.replacingOccurrences(of: "$CODE", with: code)
            return ($0.path.replacingOccurrences(of: "$CODE", with: code), try Orchd_V1_PlacementExplanation(jsonString: json))
        }
    }

    @Test func validatorMatchesExplainPlacementForEveryFixturePath() async throws {
        let cases = try Self.fixture()
        #expect(cases.count >= 8)
        // The fixture holds the cases the acceptance item names.
        #expect(cases.contains { $0.answer.roots.isEmpty && $0.answer.targetKind == "path" })
        #expect(cases.contains { $0.answer.rule == "remote_stricter" && $0.path.hasSuffix("/willesq/app") })
        #expect(Set(cases.flatMap { $0.answer.roots.map(\.kind) }) == ["public", "private", "isolated"])

        var service = FakeOrchdService()
        service.placements = Dictionary(uniqueKeysWithValues: cases.map { ($0.path, $0.answer) })
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let matched = try await Self.check(cases, with: client)
            #expect(matched == cases.count)
        }
    }

    /// Types each fixture path into the validator and compares what it
    /// shows with ExplainPlacement's answer; returns how many matched.
    @MainActor
    private static func check(_ cases: [(path: String, answer: Orchd_V1_PlacementExplanation)], with client: DaemonClient) async throws -> Int {
        var matched = 0
        let validator = PlacementValidator(api: client, home: "/Users/fixture")
        for c in cases {
            // Typed as the user would, with ~ for the home folder.
            validator.path = c.path.replacingOccurrences(of: "/Users/fixture", with: "~")
            await validator.check()
            let want = c.answer
            #expect(validator.error == nil, "\(c.path)")
            #expect(validator.checked == c.path)
            let chain = validator.chain
            #expect(chain.map(\.root.root) == want.roots.map(\.root), "\(c.path)")
            #expect(chain.map(\.root.space) == want.roots.map(\.space), "\(c.path)")
            #expect(chain.map(\.root.kind) == want.roots.map(\.kind), "\(c.path)")
            #expect(chain.last?.decides ?? true)
            #expect(chain.dropLast().allSatisfy { !$0.decides })
            let got = try #require(validator.explanation)
            #expect(got.outcome == want.outcome && got.space == want.space, "\(c.path)")
            #expect(got.rule == want.rule && got.reason == want.reason, "\(c.path)")
            #expect(got.result == (want.outcome == "conflict" ? "conflict: nothing is indexed" : want.space))
            #expect(got.remote?.effect == (want.hasRemote ? want.remote.effect : nil), "\(c.path)")
            #expect(got.remote?.space == (want.hasRemote ? want.remote.space : nil))
            #expect(got.sessionSpace == want.sessionSpace && got.sessionReads == want.sessionReads)
            if got.roots.map(\.root) == want.roots.map(\.root), got.roots.map(\.space) == want.roots.map(\.space),
               got.outcome == want.outcome, got.space == want.space, got.remote?.effect == (want.hasRemote ? want.remote.effect : nil) {
                matched += 1
            }
        }
        // A target orchd refuses shows its refusal.
        validator.path = "/elsewhere"
        await validator.check()
        #expect(validator.error == "no fixture for /elsewhere")
        return matched
    }

    @Test func enforcementRoundTrip() async throws {
        var service = FakeOrchdService()
        var sw = Orchd_V1_EnforcementSwitch()
        sw.layer = "guard"
        sw.title = "tool guard"
        sw.state = "off"
        sw.defaultState = "block-other-jobs"
        sw.states = ["off", "log", "block-other-jobs", "strict"]
        sw.relaxed = true
        sw.setAt = 1_800_000_000
        sw.setBy = "operator (process 9)"
        sw.until = 1_800_000_900
        sw.revertsTo = "block-other-jobs"
        service.enforcement.switches = [sw]
        service.enforcement.error = "orch.db differs"
        service.setEnforcement.current = sw
        service.setEnforcement.changed = true
        service.setEnforcement.notes = ["tool hooks stay on"]
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let state = try await client.enforcement()
            #expect(state.error == "orch.db differs")
            let s = try #require(state.switches.first)
            #expect(s.layer == "guard" && s.name == "tool guard" && s.relaxed)
            #expect(s.setAt == Date(timeIntervalSince1970: 1_800_000_000))
            #expect(s.until == Date(timeIntervalSince1970: 1_800_000_900))
            #expect(s.revertsTo == "block-other-jobs" && s.setBy == "operator (process 9)")
            let result = try await client.setEnforcement(layer: "guard", state: "off", forSeconds: 900)
            #expect(result.changed && result.notes == ["tool hooks stay on"] && result.current == s)
            let sent = try #require(service.recorder.last(Orchd_V1_SetEnforcementRequest.self))
            #expect(sent.layer == "guard" && sent.state == "off" && sent.forSeconds == 900)
        }
    }
}
