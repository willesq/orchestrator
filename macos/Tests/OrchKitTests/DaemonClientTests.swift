import Foundation
import Testing
@testable import OrchKit

@Suite struct DaemonClientTests {
    @Test func missingSocketFailsFast() async throws {
        let dir = try TempDir()
        let client = try DaemonClient(socketPath: dir.url.appendingPathComponent("orchd.sock").path, callTimeout: .seconds(1))
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let started = ContinuousClock.now
        await #expect(throws: (any Error).self) { try await client.status() }
        #expect(ContinuousClock.now - started < .seconds(5))
    }
}

/// Runs against a real orchd only when ORCH_LIVE_SOCKET names its socket, e.g.
/// `ORCH_LIVE_SOCKET=~/.local/share/orch/orchd.sock swift test`.
@Suite(.enabled(if: ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"] != nil))
struct LiveDaemonTests {
    @Test func statusFromRunningDaemon() async throws {
        let socket = ProcessInfo.processInfo.environment["ORCH_LIVE_SOCKET"]!
        let client = try DaemonClient(socketPath: socket)
        let runner = Task { try? await client.run() }
        defer {
            client.close()
            runner.cancel()
        }
        let status = try await client.status()
        #expect(!status.version.isEmpty)
        #expect(status.subsystems.contains { $0.name == "pr-babysitter" })
    }
}

@Suite struct DaemonClientRoundTripTests {
    @Test func mapsStatusResponse() async throws {
        var response = Orchd_V1_StatusResponse()
        response.version = "0.18.0"
        response.uptimeSeconds = 90
        var unit = Orchd_V1_SubsystemStatus()
        unit.name = "pr-babysitter"
        unit.state = "failed"
        unit.detail = "open-PR polling via gh"
        unit.error = "gh: not logged in"
        unit.restarts = 2
        response.subsystems = [unit]

        try await withFakeOrchd(FakeOrchdService(status: response)) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let status = try await client.status()
            #expect(status == DaemonStatus(version: "0.18.0", uptimeSeconds: 90, subsystems: [
                Subsystem(name: "pr-babysitter", state: "failed", detail: "open-PR polling via gh",
                          error: "gh: not logged in", restarts: 2),
            ]))
            #expect(Health(status: status) == .degraded(status, failed: ["pr-babysitter"]))
        }
    }
}

@Suite struct ListPRsRoundTripTests {
    @Test func mapsPullRequestsAndCacheTime() async throws {
        var failing = Orchd_V1_PullRequest()
        failing.url = "https://github.com/o/r/pull/1"
        failing.title = "fix"
        failing.repo = "o/r"
        failing.ciState = "failure"
        failing.reviewDecision = "APPROVED"
        failing.mergeable = "MERGEABLE"
        failing.reasons = ["authored"]
        failing.attention = "ci_failed"
        var quiet = Orchd_V1_PullRequest()
        quiet.url = "https://github.com/o/r/pull/2"
        quiet.isDraft = true
        quiet.attention = ""
        var future = Orchd_V1_PullRequest()
        future.url = "https://github.com/o/r/pull/3"
        future.attention = "some_new_reason"
        var response = Orchd_V1_ListPRsResponse()
        response.prs = [failing, quiet, future]
        response.updatedAt = 1_800_000_000

        try await withFakeOrchd(FakeOrchdService(prs: response)) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let list = try await client.listPRs()
            #expect(list.updatedAt == Date(timeIntervalSince1970: 1_800_000_000))
            #expect(list.prs.first == PullRequest(
                url: "https://github.com/o/r/pull/1", title: "fix", repo: "o/r", ciState: "failure",
                reviewDecision: "APPROVED", mergeable: "MERGEABLE", reasons: ["authored"], attention: .ciFailed
            ))
            #expect(list.prs[1].isDraft && list.prs[1].attention == nil)
            #expect(list.prs[2].attention == nil, "an unknown reason must not crash or be guessed")
            #expect(list.needsYou.map(\.url) == ["https://github.com/o/r/pull/1"])
        }
    }

    @Test func neverWrittenCacheHasNoTime() async throws {
        try await withFakeOrchd(FakeOrchdService()) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            #expect(try await client.listPRs() == PRList(prs: [], updatedAt: nil))
        }
    }
}
