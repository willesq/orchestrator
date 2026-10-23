import Foundation
import Testing
@testable import OrchKit

private func pr(_ n: Int, _ attention: Attention?) -> PullRequest {
    PullRequest(url: "https://github.com/o/r/pull/\(n)", title: "pr \(n)", repo: "o/r", attention: attention)
}

@Suite struct PullRequestTests {
    @Test(arguments: [
        ("https://github.com/o/r/pull/1", true),
        ("http://github.com/o/r/pull/1", false),
        ("https://evil.example/o/r/pull/1", false),
        ("https://github.com.evil.example/o/r/pull/1", false),
        ("https://user:pw@github.com/o/r/pull/1", false),
        ("file:///etc/passwd", false),
        ("javascript:alert(1)", false),
        ("", false),
    ])
    func webURLOnlyForGitHubHTTPS(raw: String, allowed: Bool) {
        let url = PullRequest(url: raw, title: "", repo: "").webURL
        #expect((url != nil) == allowed)
        if allowed { #expect(url?.absoluteString == raw) }
    }

    @Test func everyAttentionHasDistinctLabelAndSymbol() {
        let all = Attention.allCases
        #expect(Set(all.map(\.label)).count == all.count)
        #expect(Set(all.map(\.symbolName)).count == all.count)
        #expect(all.map(\.rawValue) == ["ci_failed", "changes_requested", "conflicts", "review_requested", "ready_to_merge"])
    }

    @Test func needsYouKeepsOrderAndDropsQuietPRs() {
        let list = PRList(prs: [pr(1, .ciFailed), pr(2, nil), pr(3, .reviewRequested)], updatedAt: nil)
        #expect(list.needsYou.map(\.url) == [pr(1, nil).url, pr(3, nil).url])
    }
}

@Suite struct NewlyNeedingYouTests {
    @Test func firstLoadIsNeverNew() {
        #expect(newlyNeedingYou(previous: nil, current: [pr(1, .ciFailed)]).isEmpty)
    }

    @Test func newPRIsNew() {
        let fresh = newlyNeedingYou(previous: [pr(1, .ciFailed)], current: [pr(1, .ciFailed), pr(2, .reviewRequested)])
        #expect(fresh.map(\.url) == [pr(2, nil).url])
    }

    @Test func newReasonOnSamePRIsNew() {
        let fresh = newlyNeedingYou(previous: [pr(1, .reviewRequested)], current: [pr(1, .readyToMerge)])
        #expect(fresh == [pr(1, .readyToMerge)])
    }

    @Test func unchangedOrResolvedIsNotNew() {
        #expect(newlyNeedingYou(previous: [pr(1, .ciFailed)], current: [pr(1, .ciFailed)]).isEmpty)
        #expect(newlyNeedingYou(previous: [pr(1, .ciFailed)], current: []).isEmpty)
        #expect(newlyNeedingYou(previous: [], current: [pr(2, nil)]).isEmpty)
    }

    @Test func flaggedAgainAfterClearingIsNew() {
        #expect(newlyNeedingYou(previous: [], current: [pr(1, .ciFailed)]) == [pr(1, .ciFailed)])
    }
}

@Suite struct TrayIconTests {
    private let status = DaemonStatus(version: "1", uptimeSeconds: 1, subsystems: [])

    @Test func healthyAndQuietShowsTheBrandMark() {
        let icon = TrayIcon(health: .healthy(status), needsYou: 0)
        #expect(icon.mark == .brand && icon.count == nil)
    }

    @Test func healthyWithPRsShowsTheMarkAndCount() {
        let icon = TrayIcon(health: .healthy(status), needsYou: 3)
        #expect(icon.mark == .brand && icon.count == 3)
    }

    @Test func degradedKeepsWarningButStillCounts() {
        let degraded = Health.degraded(status, failed: ["qdrant"])
        let icon = TrayIcon(health: degraded, needsYou: 2)
        #expect(icon.mark == .symbol(degraded.symbolName) && icon.count == 2)
    }

    @Test(arguments: [Health.down(reason: "unavailable"), Health.unknown])
    func downOrUnknownHidesStaleCount(health: Health) {
        let icon = TrayIcon(health: health, needsYou: 5)
        #expect(icon.mark == .symbol(health.symbolName) && icon.count == nil)
    }
}

@Suite struct WebURLHardeningTests {
    @Test(arguments: [
        "https://github.com:8443/o/r/pull/1",
        "https://github.com/o/r/pull/1?x=1",
        "https://github.com/o/r/pull/1#frag",
        "https://github.com/login/oauth/authorize",
        "https://github.com/o/r/issues/1",
        "https://github.com/o/r/pull/1/files",
        "https://github.com/o/r/pull/abc",
        "https://GITHUB.com/o/r/pull/1",
    ])
    func rejected(raw: String) {
        #expect(PullRequest(url: raw, title: "", repo: "").webURL == nil)
    }

    @Test func dotsAndDashesInNamesAreFine() {
        #expect(PullRequest(url: "https://github.com/will-esq/my.repo_2/pull/42", title: "", repo: "").webURL != nil)
    }
}

@Suite struct NotificationPlanTests {
    private let prs = (1...5).map { PullRequest(url: "https://github.com/o/r/pull/\($0)", title: "", repo: "o/r", attention: .reviewRequested) }

    @Test func upToTheLimitPostsEach() {
        #expect(NotificationPlan(Array(prs.prefix(3))) == .each(Array(prs.prefix(3))))
        #expect(NotificationPlan([]) == .each([]))
    }

    @Test func overTheLimitPostsOneSummary() {
        #expect(NotificationPlan(Array(prs.prefix(4))) == .summary(count: 4))
        #expect(NotificationPlan(prs, limit: 10) == .each(prs))
    }
}
