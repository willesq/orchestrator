import Testing
@testable import OrchKit

@Suite struct HealthTests {
    private func status(_ units: [Subsystem]) -> DaemonStatus {
        DaemonStatus(version: "0.17.1", uptimeSeconds: 5_460, subsystems: units)
    }

    @Test func allRunningIsHealthy() {
        let s = status([Subsystem(name: "qdrant", state: "running")])
        #expect(Health(status: s) == .healthy(s))
        #expect(Health(status: s).summary == "orchd 0.17.1 running, up 1h31m")
    }

    @Test func spacesSwitch() {
        #expect(DaemonStatus(version: "1", uptimeSeconds: 1, subsystems: [], spacesState: "off").spacesOff)
        #expect(!DaemonStatus(version: "1", uptimeSeconds: 1, subsystems: [], spacesState: "turning_on").spacesOff)
        #expect(!DaemonStatus(version: "1", uptimeSeconds: 1, subsystems: [], spacesState: "on").spacesOff)
        // An orchd that predates the switch always enforces.
        #expect(!status([]).spacesOff)
    }

    @Test func stoppedUnitIsNotDegraded() {
        let s = status([Subsystem(name: "workflow-worker", state: "stopped")])
        #expect(Health(status: s) == .healthy(s))
    }

    @Test func failedStateDegrades() {
        let s = status([
            Subsystem(name: "qdrant", state: "running"),
            Subsystem(name: "pr-babysitter", state: "failed"),
        ])
        #expect(Health(status: s) == .degraded(s, failed: ["pr-babysitter"]))
        #expect(Health(status: s).summary == "Degraded: pr-babysitter failed")
    }

    @Test func recordedErrorDegradesEvenWhileRunning() {
        let s = status([Subsystem(name: "handoff", state: "running", error: "stop timed out")])
        #expect(Health(status: s) == .degraded(s, failed: ["handoff"]))
    }

    @Test func statusOnlyWhenReachable() {
        let s = status([])
        #expect(Health.healthy(s).status == s)
        #expect(Health.degraded(s, failed: ["x"]).status == s)
        #expect(Health.down(reason: "unavailable").status == nil)
        #expect(Health.unknown.status == nil)
    }

    @Test func eachStateHasDistinctSymbolAndSummary() {
        let s = status([])
        let all: [Health] = [.unknown, .healthy(s), .degraded(s, failed: ["x"]), .down(reason: "timed out")]
        #expect(Set(all.map(\.symbolName)).count == all.count)
        #expect(Health.down(reason: "timed out").summary == "orchd is not reachable (timed out)")
        #expect(Health.unknown.summary == "Checking orchd…")
    }

    @Test(arguments: [
        (Int64(-5), "0s"), (42, "42s"), (300, "5m"), (5_460, "1h31m"), (90_000, "1d1h"),
    ])
    func uptime(seconds: Int64, expected: String) {
        #expect(formatUptime(seconds) == expected)
    }
}
