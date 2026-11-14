import Foundation
import Testing
@testable import OrchKit

private struct ListError: Error, CustomStringConvertible {
    var description: String { "RPCError: unavailable" }
}

private final class ScriptedPRs: DaemonAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var script: [Result<PRList, ListError>]
    private(set) var calls = 0

    init(_ script: [Result<PRList, ListError>]) { self.script = script }

    func status() async throws -> DaemonStatus { DaemonStatus(version: "", uptimeSeconds: 0, subsystems: []) }

    func listPRs() async throws -> PRList {
        let next: Result<PRList, ListError> = lock.withLock {
            calls += 1
            return script.count > 1 ? script.removeFirst() : script[0]
        }
        return try next.get()
    }
}

private func pr(_ n: Int, _ attention: Attention?) -> PullRequest {
    PullRequest(url: "https://github.com/o/r/pull/\(n)", title: "pr \(n)", repo: "o/r", attention: attention)
}

private func list(_ prs: PullRequest...) -> PRList { PRList(prs: prs, updatedAt: nil) }

@MainActor
@Suite struct PRMonitorTests {
    @Test func startsEmpty() {
        let monitor = PRMonitor(api: ScriptedPRs([.success(list())]))
        #expect(monitor.list == nil && monitor.needsYou.isEmpty && monitor.lastError == nil)
    }

    @Test func refreshPublishesNeedsYou() async {
        let monitor = PRMonitor(api: ScriptedPRs([.success(list(pr(1, .ciFailed), pr(2, nil)))]))
        await monitor.refresh()
        #expect(monitor.needsYou == [pr(1, .ciFailed)])
    }

    @Test func notifiesOnlyForNewItemsAfterTheFirstLoad() async {
        let api = ScriptedPRs([
            .success(list(pr(1, .ciFailed))),
            .success(list(pr(1, .ciFailed), pr(2, .reviewRequested))),
            .success(list(pr(1, .ciFailed), pr(2, .reviewRequested))),
        ])
        let monitor = PRMonitor(api: api)
        var notified: [[PullRequest]] = []
        monitor.onNewlyNeedingYou = { notified.append($0) }
        await monitor.refresh()
        await monitor.refresh()
        await monitor.refresh()
        #expect(notified == [[pr(2, .reviewRequested)]])
    }

    @Test func failureKeepsLastGoodListAndRecovers() async {
        let api = ScriptedPRs([.success(list(pr(1, .ciFailed))), .failure(ListError()), .success(list())])
        let monitor = PRMonitor(api: api)
        await monitor.refresh()
        await monitor.refresh()
        #expect(monitor.needsYou == [pr(1, .ciFailed)])
        #expect(monitor.lastError == "unavailable")
        await monitor.refresh()
        #expect(monitor.lastError == nil && monitor.needsYou.isEmpty)
    }

    @Test func failureBeforeAnyLoadDoesNotNotify() async {
        let api = ScriptedPRs([.failure(ListError()), .success(list(pr(1, .ciFailed)))])
        let monitor = PRMonitor(api: api)
        var notified = 0
        monitor.onNewlyNeedingYou = { _ in notified += 1 }
        await monitor.refresh()
        await monitor.refresh()
        #expect(notified == 0, "the first successful load is still a first load")
    }

    @Test func startPollsAndIsIdempotent() async throws {
        let api = ScriptedPRs([.success(list())])
        let monitor = PRMonitor(api: api, interval: .milliseconds(20))
        monitor.start()
        monitor.start()
        try await eventually { api.calls >= 2 }
        monitor.stop()
        let polled = api.calls
        #expect(polled >= 2)
        try await Task.sleep(for: .milliseconds(80))
        #expect(api.calls <= polled + 1, "polling continued after stop")
    }
}
