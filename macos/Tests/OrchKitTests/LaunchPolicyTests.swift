import Foundation
import Testing
@testable import OrchKit

@Suite struct LaunchPolicyTests {
    @Test func onlyAUserLaunchOpensTheWindow() {
        #expect(LaunchPolicy.opensWindowAtLaunch(launchedAsLoginItem: false))
        #expect(!LaunchPolicy.opensWindowAtLaunch(launchedAsLoginItem: true))
    }

    @Test func notificationNameIsNamespaced() {
        #expect(LaunchPolicy.showWindowNotification.rawValue == "com.willesq.orch.show-main-window")
    }

    private let t0 = Date(timeIntervalSince1970: 1_000)

    @Test func noOtherCopyMeansStay() {
        let me = LaunchPolicy.Copy(pid: 10, launched: t0)
        #expect(!LaunchPolicy.shouldHandOff(me: me, others: [me]))
        #expect(!LaunchPolicy.shouldHandOff(me: me, others: []))
    }

    @Test func laterCopyHandsOffToEarlierOne() {
        let first = LaunchPolicy.Copy(pid: 20, launched: t0)
        let second = LaunchPolicy.Copy(pid: 10, launched: t0.addingTimeInterval(5))
        #expect(LaunchPolicy.shouldHandOff(me: second, others: [first, second]))
        #expect(!LaunchPolicy.shouldHandOff(me: first, others: [first, second]))
    }

    @Test func simultaneousLaunchKeepsExactlyOne() {
        let a = LaunchPolicy.Copy(pid: 11, launched: t0)
        let b = LaunchPolicy.Copy(pid: 12, launched: t0)
        let all = [a, b]
        let survivors = all.filter { !LaunchPolicy.shouldHandOff(me: $0, others: all) }
        #expect(survivors == [a])
    }

    @Test func unknownLaunchDatesFallBackToPid() {
        let a = LaunchPolicy.Copy(pid: 11, launched: nil)
        let b = LaunchPolicy.Copy(pid: 12, launched: nil)
        let c = LaunchPolicy.Copy(pid: 13, launched: t0)
        #expect(!LaunchPolicy.shouldHandOff(me: a, others: [a, b]))
        #expect(LaunchPolicy.shouldHandOff(me: b, others: [a, b]))
        #expect(LaunchPolicy.shouldHandOff(me: a, others: [a, c]), "a known launch date outranks an unknown one")
        #expect(!LaunchPolicy.shouldHandOff(me: c, others: [a, c]))
    }

    @Test func terminatedCopiesDoNotCount() {
        let dying = LaunchPolicy.Copy(pid: 5, launched: t0, terminated: true)
        let me = LaunchPolicy.Copy(pid: 6, launched: t0.addingTimeInterval(1))
        #expect(!LaunchPolicy.shouldHandOff(me: me, others: [dying, me]))
    }

    @Test func requestsAreRateLimited() {
        #expect(LaunchPolicy.honours(request: t0, lastHonoured: nil))
        #expect(!LaunchPolicy.honours(request: t0.addingTimeInterval(0.5), lastHonoured: t0))
        #expect(LaunchPolicy.honours(request: t0.addingTimeInterval(1), lastHonoured: t0))
    }
}
