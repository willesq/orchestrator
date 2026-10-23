import Foundation
import Testing
@testable import OrchKit

private struct StubError: Error, CustomStringConvertible {
    let description: String
}

private final class FakeAPI: DaemonAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var results: [Result<DaemonStatus, StubError>]
    private(set) var calls = 0

    init(_ results: [Result<DaemonStatus, StubError>]) { self.results = results }

    func status() async throws -> DaemonStatus {
        let next: Result<DaemonStatus, StubError> = lock.withLock {
            calls += 1
            return results.count > 1 ? results.removeFirst() : results[0]
        }
        return try next.get()
    }
}

@MainActor
@Suite struct DaemonMonitorTests {
    private let ok = DaemonStatus(version: "1.0", uptimeSeconds: 1, subsystems: [])

    @Test func startsUnknown() {
        #expect(DaemonMonitor(api: FakeAPI([.success(ok)])).health == .unknown)
    }

    @Test func refreshPublishesHealth() async {
        let monitor = DaemonMonitor(api: FakeAPI([.success(ok)]))
        await monitor.refresh()
        #expect(monitor.health == .healthy(ok))
        #expect(monitor.lastChecked != nil)
    }

    @Test func failureMarksDown() async {
        let monitor = DaemonMonitor(api: FakeAPI([.failure(StubError(description: "RPCError: unavailable (connect failed)"))]))
        await monitor.refresh()
        #expect(monitor.health == .down(reason: "unavailable"))
    }

    @Test func recoversAfterFailure() async {
        let monitor = DaemonMonitor(api: FakeAPI([.failure(StubError(description: "x")), .success(ok)]))
        await monitor.refresh()
        await monitor.refresh()
        #expect(monitor.health == .healthy(ok))
    }

    @Test func startPollsAndIsIdempotent() async throws {
        let api = FakeAPI([.success(ok)])
        let monitor = DaemonMonitor(api: api, interval: .milliseconds(20))
        monitor.start()
        monitor.start()
        try await Task.sleep(for: .milliseconds(150))
        monitor.stop()
        let polled = api.calls
        #expect(polled >= 2)
        #expect(monitor.health == .healthy(ok))
        try await Task.sleep(for: .milliseconds(80))
        #expect(api.calls <= polled + 1, "polling continued after stop")
    }

    @Test(arguments: [
        ("status: deadline exceeded", "timed out"),
        ("short", "short"),
        (String(repeating: "a", count: 100), String(repeating: "a", count: 80) + "…"),
    ])
    func describe(raw: String, expected: String) {
        #expect(DaemonMonitor.describe(StubError(description: raw)) == expected)
    }
}
