import Foundation
import Testing
@testable import OrchKit

/// A temporary spaces stream.
private final class StreamFile {
    let dir: URL
    let path: String

    init() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("orch-refusals-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        path = dir.appendingPathComponent("spaces.jsonl").path
    }

    deinit { try? FileManager.default.removeItem(at: dir) }

    func append(_ s: String) throws {
        if !FileManager.default.fileExists(atPath: path) {
            FileManager.default.createFile(atPath: path, contents: nil)
        }
        let h = try #require(FileHandle(forWritingAtPath: path))
        try h.seekToEnd()
        try h.write(contentsOf: Data(s.utf8))
        try h.close()
    }

    func replace(_ s: String) throws {
        try Data(s.utf8).write(to: URL(fileURLWithPath: path))
    }
}

// As orch-hook writes them (internal/events NewRefusal).
private let guardLine = #"{"kind":"refusal","source":"guard","summary":"refused a Bash call (rule other_job_path) in space acme, session s1","timestamp":"2026-11-14T08:30:00Z","refusal":{"session":"s1","space":"acme","tool":"Bash","rule":"other_job_path"}}"# + "\n"
private let termLine = #"{"kind":"refusal","source":"guard","summary":"x","timestamp":"2026-11-14T08:31:00Z","refusal":{"session":"s2","space":"acme","tool":"Bash","rule":"outbound_term","term":{"space":"globex","line":3,"hash":"0123456789ab"}}}"# + "\n"
private let unpinnedLine = #"{"kind":"refusal","source":"guard","summary":"x","timestamp":"2026-11-14T08:32:00Z","refusal":{"space":"","tool":"Write","rule":"pin_missing"}}"# + "\n"
private let infoLine = #"{"kind":"info","source":"enforcement","summary":"guard switched off","timestamp":"2026-11-14T08:29:00Z"}"# + "\n"

@Suite struct RefusalStreamTests {
    @Test func parsesRefusalsAndSkipsOtherEvents() throws {
        let f = try StreamFile()
        try f.append(infoLine + guardLine + termLine + unpinnedLine)
        let (found, next) = RefusalStream(path: f.path).read(from: 0)
        #expect(found.count == 3)
        #expect(found[0] == Refusal(session: "s1", space: "acme", tool: "Bash", rule: "other_job_path",
                                    at: try Date("2026-11-14T08:30:00Z", strategy: .iso8601)))
        #expect(found[1].term == Refusal.TermRef(space: "globex", line: 3, hash: "0123456789ab"))
        #expect(found[2].session.isEmpty && found[2].space.isEmpty && found[2].rule == "pin_missing")
        #expect(next == RefusalStream(path: f.path).end())
    }

    @Test func readsOnlyWhatFollowsTheOffsetAndOnlyWholeLines() throws {
        let f = try StreamFile()
        let stream = RefusalStream(path: f.path)
        try f.append(guardLine)
        let start = stream.end()
        try f.append(String(termLine.dropLast(5)))
        var (found, next) = stream.read(from: start)
        #expect(found.isEmpty && next == start)
        try f.append(String(termLine.suffix(5)))
        (found, next) = stream.read(from: next)
        #expect(found.map(\.session) == ["s2"] && next == stream.end())
        (found, next) = stream.read(from: next)
        #expect(found.isEmpty && next == stream.end())
    }

    @Test func aReplacedStreamIsReadFromItsStart() throws {
        let f = try StreamFile()
        let stream = RefusalStream(path: f.path)
        try f.append(guardLine + termLine)
        let offset = stream.end()
        try f.replace(unpinnedLine)
        let (found, next) = stream.read(from: offset)
        #expect(found.map(\.rule) == ["pin_missing"] && next == UInt64(unpinnedLine.utf8.count))
    }

    @Test func aMissingStreamReadsNothing() {
        let stream = RefusalStream(path: "/nonexistent/orch/events/spaces.jsonl")
        #expect(stream.end() == 0)
        let (found, next) = stream.read(from: 40)
        #expect(found.isEmpty && next == 0)
    }

    @Test func anOverlongLineIsSkipped() throws {
        let f = try StreamFile()
        var stream = RefusalStream(path: f.path)
        stream.maxRead = 64
        try f.append(String(repeating: "x", count: 100))
        let (found, next) = stream.read(from: 0)
        #expect(found.isEmpty && next == 64)
    }

    @Test(arguments: [
        #"{"kind":"refusal","refusal":{"session":"../x","space":"acme","tool":"Bash","rule":"r"}}"#,
        #"{"kind":"refusal","refusal":{"space":"Acme Corp","tool":"Bash","rule":"r"}}"#,
        #"{"kind":"refusal","refusal":{"space":"acme","tool":"Bash; rm -rf /","rule":"r"}}"#,
        #"{"kind":"refusal","refusal":{"space":"acme","tool":"Bash","rule":"/Users/x/secret"}}"#,
        #"{"kind":"refusal","refusal":{"space":"acme","tool":"Bash","rule":"outbound_term","term":{"space":"globex","line":0,"hash":"ab"}}}"#,
        #"{"kind":"refusal","refusal":{"space":"acme","tool":"Bash","rule":"outbound_term","term":{"space":"globex","line":1,"hash":"zephyr"}}}"#,
        #"{"kind":"refusal"}"#,
        #"not json"#,
    ])
    func rejectsFieldsOrchWouldNotWrite(_ line: String) {
        #expect(RefusalStream.parse(Data(line.utf8)) == nil)
    }

    @Test func defaultPathFollowsTheDataDir() {
        let stream = RefusalStream.spaces(environment: ["ORCH_DATA_DIR": "/tmp/orch-sp-048"], home: "/Users/u")
        #expect(stream.path == "/tmp/orch-sp-048/events/spaces.jsonl")
    }

    @Test func refusalText() {
        let r = Refusal(session: "s2", space: "acme", tool: "Bash", rule: "outbound_term",
                        term: .init(space: "globex", line: 3, hash: "0123456789ab"))
        #expect(r.place == "space acme · Bash")
        #expect(r.detail == "Rule outbound_term in session s2; a term of space globex, terms file line 3 (hash 0123456789ab).")
        let bare = Refusal(session: "", space: "", tool: "Write", rule: "pin_missing")
        #expect(bare.place == "no space · Write" && bare.detail == "Rule pin_missing.")
    }
}

@MainActor
@Suite struct RefusalMonitorTests {
    @Test func announcesOnlyRefusalsMadeAfterTheFirstPoll() throws {
        let f = try StreamFile()
        try f.append(guardLine)
        let monitor = RefusalMonitor(stream: RefusalStream(path: f.path))
        var announced: [[String]] = []
        monitor.onNewRefusals = { announced.append($0.map(\.session)) }
        monitor.poll()
        monitor.poll()
        try f.append(termLine + infoLine)
        monitor.poll()
        monitor.poll()
        #expect(announced == [["s2"]])
        #expect(monitor.recent.map(\.session) == ["s2"])
    }

    @Test func keepsTheLatestRefusals() throws {
        let f = try StreamFile()
        let monitor = RefusalMonitor(stream: RefusalStream(path: f.path))
        monitor.poll()
        try f.append(String(repeating: guardLine, count: 25) + termLine)
        monitor.poll()
        #expect(monitor.recent.count == monitor.kept && monitor.recent.last?.session == "s2")
    }

    @Test func startPollsUntilStopped() async throws {
        let f = try StreamFile()
        let monitor = RefusalMonitor(stream: RefusalStream(path: f.path), interval: .milliseconds(10))
        var announced = 0
        monitor.onNewRefusals = { announced += $0.count }
        monitor.start()
        monitor.start()
        try await Task.sleep(for: .milliseconds(50))
        try f.append(guardLine)
        for _ in 0..<100 where announced == 0 {
            try await Task.sleep(for: .milliseconds(10))
        }
        monitor.stop()
        #expect(announced == 1)
    }
}

@Suite struct RefusalNotificationPlanTests {
    private func r(_ space: String) -> Refusal { Refusal(session: "s", space: space, tool: "Bash", rule: "other_job_path") }

    @Test func eachUpToTheLimitThenOneSummary() {
        #expect(RefusalNotificationPlan([r("acme"), r("acme")]) == .each([r("acme"), r("acme")]))
        #expect(RefusalNotificationPlan([r("acme"), r(""), r("acme"), r("globex")])
            == .summary(count: 4, spaces: ["acme", "no space", "globex"]))
    }
}
