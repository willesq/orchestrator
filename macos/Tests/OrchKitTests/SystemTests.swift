import Foundation
import Testing
@testable import OrchKit

private struct Down: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private final class FakeSystem: SystemAPI, @unchecked Sendable {
    private let lock = NSLock()
    var pages: [LogPage] = []
    var metrics: [SystemMetrics] = []
    var status = DaemonStatus(version: "0.20.0", uptimeSeconds: 60, subsystems: [])
    var fail = false
    /// Runs inside tailLogs, so a test can act while a call is out.
    var duringTail: (@MainActor () -> Void)?
    private(set) var tails: [(after: UInt64, limit: Int)] = []

    func status() async throws -> DaemonStatus {
        if fail { throw Down() }
        return status
    }

    func tailLogs(after: UInt64, limit: Int) async throws -> LogPage {
        lock.withLock { tails.append((after, limit)) }
        if let duringTail { await duringTail() }
        if fail { throw Down() }
        return lock.withLock { pages.isEmpty ? LogPage(lines: [], lastSeq: after, oldestSeq: 1, sources: []) : pages.removeFirst() }
    }

    func systemMetrics() async throws -> SystemMetrics {
        if fail { throw Down() }
        return lock.withLock { metrics.count > 1 ? metrics.removeFirst() : metrics[0] }
    }
}

private func line(_ seq: UInt64, _ source: String = "orchd", _ message: String = "m") -> LogLine {
    LogLine(seq: seq, at: Date(timeIntervalSince1970: TimeInterval(seq)), source: source, message: message)
}

private func page(_ seqs: ClosedRange<UInt64>, last: UInt64? = nil, oldest: UInt64 = 1, source: String = "orchd") -> LogPage {
    LogPage(lines: seqs.map { line($0, source) }, lastSeq: last ?? seqs.upperBound, oldestSeq: oldest,
            sources: ["orchd", source].sorted())
}

@MainActor
@Suite struct LogsModelTests {
    @Test func firstPollAsksForRecentLinesThenFollows() async {
        let api = FakeSystem()
        api.pages = [page(1...3), page(4...5)]
        let model = LogsModel(api: api)
        await model.poll()
        await model.poll()
        #expect(model.lines.map(\.seq) == [1, 2, 3, 4, 5])
        #expect(api.tails.map(\.after) == [0, 3])
        #expect(api.tails.map(\.limit) == [1000, LogsModel.capacity])
        #expect(model.missed == 0)
        #expect(model.lastError == nil)
    }

    @Test func countsLinesThatRolledOut() async {
        let api = FakeSystem()
        api.pages = [page(1...3), page(10...12, oldest: 10)]
        let model = LogsModel(api: api)
        await model.poll()
        await model.poll()
        #expect(model.missed == 6)
        #expect(model.lines.last?.seq == 12)
    }

    @Test func orchdRestartStartsOver() async {
        let api = FakeSystem()
        api.pages = [page(1...50), page(1...2)]
        let model = LogsModel(api: api)
        await model.poll()
        await model.poll()
        #expect(model.lines.map(\.seq) == [1, 2])
        #expect(model.missed == 0)
    }

    @Test func keepsAtMostCapacity() {
        let model = LogsModel(api: FakeSystem())
        model.apply(page(1...UInt64(LogsModel.capacity + 10)))
        #expect(model.lines.count == LogsModel.capacity)
        #expect(model.lines.first?.seq == 11)
    }

    @Test func filtersBySourceAndText() {
        let model = LogsModel(api: FakeSystem())
        model.apply(LogPage(lines: [line(1, "orchd", "listening"), line(2, "qdrant", "Ready"), line(3, "qdrant", "flush")],
                            lastSeq: 3, oldestSeq: 1, sources: ["orchd", "qdrant"]))
        #expect(model.sources == ["orchd", "qdrant"])
        #expect(model.visibleLines.count == 3)
        model.source = "qdrant"
        #expect(model.visibleLines.map(\.seq) == [2, 3])
        model.filter = " ready "
        #expect(model.visibleLines.map(\.seq) == [2])
        model.source = nil
        #expect(model.visibleLines.map(\.seq) == [2])
    }

    @Test func pausedFetchesNothing() async {
        let api = FakeSystem()
        let model = LogsModel(api: api)
        model.paused = true
        await model.poll()
        #expect(api.tails.isEmpty)
    }

    @Test func pausingDuringACallDropsItsPage() async {
        let api = FakeSystem()
        api.pages = [page(1...3)]
        let model = LogsModel(api: api)
        api.duringTail = { model.paused = true }
        await model.poll()
        #expect(model.lines.isEmpty)
        // Resuming asks again from where the view stopped.
        model.paused = false
        api.duringTail = nil
        api.pages = [page(1...3)]
        await model.poll()
        #expect(api.tails.last?.after == 0)
        #expect(model.lines.count == 3)
    }

    @Test func clearKeepsFollowing() async {
        let api = FakeSystem()
        api.pages = [page(1...3), page(10...11, oldest: 10), page(12...12, oldest: 10)]
        let model = LogsModel(api: api)
        await model.poll()
        await model.poll()
        model.clear()
        #expect(model.lines.isEmpty && model.missed == 0)
        await model.poll()
        #expect(model.lines.map(\.seq) == [12])
    }

    @Test func repeatedLinesAreNotShownTwice() {
        let model = LogsModel(api: FakeSystem())
        model.apply(page(1...3))
        model.apply(page(2...5))
        #expect(model.lines.map(\.seq) == [1, 2, 3, 4, 5])
        #expect(model.missed == 0)
    }

    @Test func overlappingPollsAskOnce() async {
        let api = FakeSystem()
        api.pages = [page(1...3)]
        let model = LogsModel(api: api)
        api.duringTail = { Task { await model.poll() } }
        await model.poll()
        await Task.yield()
        #expect(api.tails.count == 1)
        #expect(model.lines.map(\.seq) == [1, 2, 3])
    }

    @Test func reportsFailures() async {
        let api = FakeSystem()
        api.fail = true
        let model = LogsModel(api: api)
        await model.poll()
        #expect(model.lastError == "unavailable")
        api.fail = false
        await model.poll()
        #expect(model.lastError == nil)
    }

    @Test func pollsUntilStopped() async throws {
        let api = FakeSystem()
        let model = LogsModel(api: api, interval: .milliseconds(10))
        model.start()
        model.start()
        try await Task.sleep(for: .milliseconds(100))
        model.stop()
        let count = api.tails.count
        #expect(count >= 2)
        try await Task.sleep(for: .milliseconds(50))
        // A call already in flight at stop() may still land; nothing after it.
        #expect(api.tails.count <= count + 1)
    }
}

private func metrics(at seconds: TimeInterval, qdrantRunning: Bool = true, cpu: Double = 1) -> SystemMetrics {
    SystemMetrics(
        at: Date(timeIntervalSince1970: seconds),
        processes: [
            ProcessStats(name: "orchd", pid: 10, rssBytes: 100, cpuPercent: cpu, uptimeSeconds: 5, running: true),
            ProcessStats(name: "qdrant", pid: qdrantRunning ? 11 : 0, rssBytes: qdrantRunning ? 200 : 0,
                         running: qdrantRunning),
        ],
        goroutines: 40, heapBytes: 1 << 20,
        data: [DataUsage(name: "database", path: "/d/orch.db", bytes: 4096)],
        tasks: ["done": 3]
    )
}

@MainActor
@Suite struct SystemModelTests {
    @Test func refreshKeepsMetricsStatusAndHistory() async {
        let api = FakeSystem()
        api.metrics = [metrics(at: 1, cpu: 5), metrics(at: 2, cpu: 7)]
        let model = SystemModel(api: api, loadReport: { StatusReport(version: "x", problems: 0, sections: []) })
        await model.refresh()
        await model.refresh()
        #expect(model.metrics?.process("orchd")?.cpuPercent == 7)
        #expect(model.metrics?.process("missing") == nil)
        #expect(model.status?.version == "0.20.0")
        #expect(model.history["orchd"]?.map(\.cpuPercent) == [5, 7])
        #expect(model.history["qdrant"]?.map(\.rssBytes) == [200, 200])
        #expect(model.lastError == nil)
    }

    @Test func historyIsBoundedAndResetsWhenAProcessStops() async {
        let api = FakeSystem()
        api.metrics = (0...(SystemModel.historyLength + 4)).map { metrics(at: TimeInterval($0)) }
            + [metrics(at: 1000, qdrantRunning: false)]
        let model = SystemModel(api: api, loadReport: { StatusReport(version: "x", problems: 0, sections: []) })
        for _ in 0...(SystemModel.historyLength + 4) { await model.refresh() }
        #expect(model.history["orchd"]?.count == SystemModel.historyLength)
        #expect(model.history["orchd"]?.last?.at == Date(timeIntervalSince1970: TimeInterval(SystemModel.historyLength + 4)))
        await model.refresh()
        #expect(model.history["qdrant"]?.isEmpty == true)
    }

    @Test func aNewPIDStartsANewLine() async {
        let api = FakeSystem()
        let restarted = SystemMetrics(at: Date(timeIntervalSince1970: 3), processes: [
            ProcessStats(name: "orchd", pid: 99, rssBytes: 1, cpuPercent: 9, uptimeSeconds: 1, running: true),
        ])
        api.metrics = [metrics(at: 1), metrics(at: 2), restarted]
        let model = SystemModel(api: api, loadReport: { StatusReport(version: "x", problems: 0, sections: []) })
        for _ in 0..<3 { await model.refresh() }
        #expect(model.history["orchd"]?.map(\.cpuPercent) == [9])
    }

    @Test func reportsFailuresAndRecovers() async {
        let api = FakeSystem()
        api.metrics = [metrics(at: 1)]
        api.fail = true
        let model = SystemModel(api: api, loadReport: { throw DaemonControl.ControlError.orchNotFound })
        await model.refresh()
        #expect(model.lastError == "unavailable")
        await model.refreshReport()
        #expect(model.reportError?.contains("brew install") == true)
        #expect(!model.loadingReport)
        api.fail = false
        await model.refresh()
        #expect(model.lastError == nil)
    }

    @Test func loadsTheReport() async {
        let model = SystemModel(api: FakeSystem(), loadReport: {
            StatusReport(version: "0.20.0", problems: 1, sections: [StatusReport.Section(title: "service")])
        })
        await model.refreshReport()
        #expect(model.report?.problems == 1)
        #expect(model.reportError == nil)
    }

    @Test func pollsUntilStopped() async throws {
        let api = FakeSystem()
        api.metrics = [metrics(at: 1)]
        let reports = Counter()
        let model = SystemModel(api: api, loadReport: {
            reports.increment()
            return StatusReport(version: "x", problems: 0, sections: [])
        }, interval: .milliseconds(10), reportInterval: .milliseconds(30))
        model.start()
        model.start()
        try await Task.sleep(for: .milliseconds(150))
        model.stop()
        #expect(model.metrics != nil)
        #expect(reports.value >= 2)
    }
}

private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    func increment() { lock.withLock { n += 1 } }
    var value: Int { lock.withLock { n } }
}

@Suite struct StatusReportTests {
    @Test func decodesOrchStatusJSON() throws {
        let json = """
        {"version":"0.19.0","at":"2026-10-23T19:09:17.39-07:00","problems":1,"sections":[
          {"title":"service","lines":[{"name":"orchd","value":"running","detail":"0.19.0, up 49m","level":"ok"},
                                      {"name":"socket","value":"/s/orchd.sock"}]},
          {"title":"harnesses","note":"nothing installed"}
        ]}
        """
        let report = try JSONDecoder().decode(StatusReport.self, from: Data(json.utf8))
        #expect(report.version == "0.19.0" && report.problems == 1)
        #expect(report.sections.map(\.title) == ["service", "harnesses"])
        #expect(report.sections[0].lines[0] == StatusReport.Line(name: "orchd", value: "running", detail: "0.19.0, up 49m", level: "ok"))
        #expect(report.sections[0].lines[1].level == nil)
        #expect(report.sections[1].lines.isEmpty && report.sections[1].note == "nothing installed")
    }

    @Test func runsOrchStatusJSON() async throws {
        let dir = try TempDir()
        let log = dir.url.appendingPathComponent("argv").path
        let orch = try dir.script("""
        echo "$@" > '\(log)'
        echo 'probing…' >&2
        echo '{"version":"1","problems":0,"sections":[]}'
        """)
        let report = try await DaemonControl(orchPath: orch).statusReport()
        #expect(report == StatusReport(version: "1", problems: 0, sections: []))
        #expect(try String(contentsOfFile: log, encoding: .utf8) == "status --json\n")
    }

    @Test func failingOrchStatusCarriesStderr() async throws {
        let dir = try TempDir()
        let orch = try dir.script("echo 'unknown flag: --json' >&2; exit 1")
        await #expect(throws: DaemonControl.ControlError.failed(status: 1, output: "unknown flag: --json")) {
            try await DaemonControl(orchPath: orch).statusReport()
        }
    }

    @Test func unreadableReportThrows() async throws {
        let dir = try TempDir()
        let orch = try dir.script("echo 'not json'")
        await #expect(throws: DaemonControl.ControlError.unreadable) {
            try await DaemonControl(orchPath: orch).statusReport()
        }
    }

    @Test func toleratesMissingFields() throws {
        let report = try JSONDecoder().decode(StatusReport.self, from: Data(#"{"sections":null}"#.utf8))
        #expect(report == StatusReport(version: "", problems: 0, sections: []))
    }

    @Test(.timeLimit(.minutes(1)))
    func hungOrchIsStopped() async throws {
        let dir = try TempDir()
        // A grandchild keeps the pipes open after orch itself is stopped.
        let orch = try dir.script("sleep 30 & sleep 30")
        let started = ContinuousClock.now
        await #expect(throws: DaemonControl.ControlError.timedOut) {
            try await DaemonControl(orchPath: orch).statusReport(timeout: .milliseconds(300))
        }
        #expect(ContinuousClock.now - started < .seconds(10))
    }

    @Test(.timeLimit(.minutes(1)))
    func cancellingStopsOrch() async throws {
        let dir = try TempDir()
        let orch = try dir.script("exec sleep 30")
        let task = Task { try await DaemonControl(orchPath: orch).statusReport() }
        try await Task.sleep(for: .milliseconds(300))
        let started = ContinuousClock.now
        task.cancel()
        await #expect(throws: (any Error).self) { try await task.value }
        #expect(ContinuousClock.now - started < .seconds(10))
    }

    @Test func errorOutputIsCapped() {
        let long = DaemonControl.message(Data(String(repeating: "x", count: 2000).utf8))
        #expect(long.count == 501 && long.hasSuffix("…"))
        #expect(DaemonControl.message(Data("  short \n".utf8)) == "short")
        #expect(DaemonControl.ControlError.timedOut.errorDescription?.contains("stopped") == true)
        #expect(DaemonControl.ControlError.unreadable.errorDescription?.contains("brew upgrade orch") == true)
    }

    @Test func formatsBytes() {
        #expect(formatBytes(0).hasPrefix("0 "))
        #expect(formatBytes(1_500_000).hasSuffix("MB"))
    }
}

@Suite struct SystemRoundTripTests {
    @Test func mapsLogsMetricsAndHeartbeats() async throws {
        var service = FakeOrchdService()
        var entry = Orchd_V1_LogEntry()
        entry.seq = 7
        entry.atUnixMs = 1_700_000_000_500
        entry.source = "qdrant"
        entry.message = "ready"
        service.logs.entries = [entry]
        service.logs.lastSeq = 9
        service.logs.oldestSeq = 2
        service.logs.sources = ["orchd", "qdrant"]

        var proc = Orchd_V1_ProcessMetrics()
        proc.name = "qdrant"
        proc.pid = 42
        proc.rssBytes = 512
        proc.cpuPercent = 12.5
        proc.uptimeSeconds = 30
        proc.running = true
        var size = Orchd_V1_DataSize()
        size.name = "database"
        size.path = "/d/orch.db"
        size.bytes = 4096
        service.metrics.atUnixMs = 1_700_000_001_000
        service.metrics.processes = [proc]
        service.metrics.goroutines = 33
        service.metrics.heapBytes = 2048
        service.metrics.data = [size]
        service.metrics.tasks = ["done": 4]

        var unit = Orchd_V1_SubsystemStatus()
        unit.name = "docs-scheduler"
        unit.state = "running"
        unit.startedAt = 1_700_000_000
        unit.lastBeat = 1_700_000_060
        service.status.subsystems = [unit]

        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let logs = try await client.tailLogs(after: 6, limit: 50)
            #expect(logs == LogPage(
                lines: [LogLine(seq: 7, at: Date(timeIntervalSince1970: 1_700_000_000.5), source: "qdrant", message: "ready")],
                lastSeq: 9, oldestSeq: 2, sources: ["orchd", "qdrant"]))
            let request = service.recorder.last(Orchd_V1_TailLogsRequest.self)
            #expect(request?.afterSeq == 6 && request?.limit == 50 && request?.source == "")

            let m = try await client.systemMetrics()
            #expect(m == SystemMetrics(
                at: Date(timeIntervalSince1970: 1_700_000_001),
                processes: [ProcessStats(name: "qdrant", pid: 42, rssBytes: 512, cpuPercent: 12.5, uptimeSeconds: 30, running: true)],
                goroutines: 33, heapBytes: 2048,
                data: [DataUsage(name: "database", path: "/d/orch.db", bytes: 4096)], tasks: ["done": 4]))

            let status = try await client.status()
            #expect(status.subsystems.first?.startedAt == Date(timeIntervalSince1970: 1_700_000_000))
            #expect(status.subsystems.first?.lastBeat == Date(timeIntervalSince1970: 1_700_000_060))
        }
    }
}
