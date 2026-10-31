import Foundation
import Testing
@testable import OrchKit

private let now = Date(timeIntervalSince1970: 1_800_000_000)
private let recent = now.addingTimeInterval(-3600)
private let ancient = now.addingTimeInterval(-prColdAge - 86400)

private func pr(_ number: Int, updated: Date? = recent, draft: Bool = false, ci: String = "",
                attention: Attention? = nil, isPR: Bool = true) -> GitHubItem {
    GitHubItem(url: "https://github.com/willesq/orchestrator/pull/\(number)", title: "PR \(number)",
               repo: "willesq/orchestrator", isPR: isPR, number: number, updatedAt: updated, isDraft: draft,
               ciState: ci, attention: attention)
}

@Suite struct PRBoardTests {
    @Test func attentionPicksTheLane() {
        #expect(prLane(pr(1, attention: .ciFailed), review: nil, now: now) == .fix)
        #expect(prLane(pr(1, attention: .changesRequested), review: nil, now: now) == .fix)
        #expect(prLane(pr(1, attention: .conflicts), review: nil, now: now) == .fix)
        #expect(prLane(pr(1, attention: .readyToMerge), review: nil, now: now) == .ship)
        #expect(prLane(pr(1, attention: .reviewRequested), review: nil, now: now) == .review)
    }

    @Test func ciAndDraftWithoutAttention() {
        #expect(prLane(pr(1, ci: "failure"), review: nil, now: now) == .fix)
        // A failing draft still needs fixing before it can move on.
        #expect(prLane(pr(1, draft: true, ci: "failure"), review: nil, now: now) == .fix)
        #expect(prLane(pr(1, draft: true), review: nil, now: now) == .flight)
        #expect(prLane(pr(1, ci: "pending"), review: nil, now: now) == .flight)
        #expect(prLane(pr(1, ci: "success"), review: nil, now: now) == .review)
        #expect(prLane(pr(1), review: nil, now: now) == .review)
    }

    @Test func verdictsCountOnlyForTheCurrentHead() {
        let url = pr(1).url
        let approve = PRReview(url: url, state: .verdict, verdict: .approve)
        let changes = PRReview(url: url, state: .verdict, verdict: .changes)
        let wait = PRReview(url: url, state: .verdict, verdict: .wait)
        #expect(prLane(pr(1, ci: "success"), review: approve, now: now) == .ship)
        #expect(prLane(pr(1, ci: "success"), review: changes, now: now) == .fix)
        #expect(prLane(pr(1, ci: "success"), review: wait, now: now) == .review)
        // A verdict on an older head, with a new review under way, moves nothing.
        let moved = PRReview(url: url, state: .reviewing, verdict: .approve)
        #expect(prLane(pr(1, ci: "success"), review: moved, now: now) == .review)
        // CI failing beats an approval.
        #expect(prLane(pr(1, attention: .ciFailed), review: approve, now: now) == .fix)
    }

    @Test func oldPRsGoColdUnlessSomeoneWaits() {
        #expect(prLane(pr(1, updated: ancient, attention: .conflicts), review: nil, now: now) == .cold)
        #expect(prLane(pr(1, updated: ancient, draft: true), review: nil, now: now) == .cold)
        #expect(prLane(pr(1, updated: ancient, attention: .reviewRequested), review: nil, now: now) == .review)
        #expect(prLane(pr(1, updated: nil), review: nil, now: now) == .review)
        #expect(prLane(pr(1, updated: now.addingTimeInterval(-prColdAge + 60)), review: nil, now: now) == .review)
    }

    @Test func boardGroupsPRsKeepingOrder() {
        let items = [pr(1, attention: .ciFailed), pr(2, draft: true), pr(3, attention: .ciFailed),
                     pr(4, isPR: false), pr(5, updated: ancient)]
        let lanes = prBoard(items, reviews: [:], now: now)
        #expect(lanes[.fix]?.map(\.number) == [1, 3])
        #expect(lanes[.flight]?.map(\.number) == [2])
        #expect(lanes[.cold]?.map(\.number) == [5])
        #expect(lanes[.review] == nil)
        #expect(lanes.values.flatMap { $0 }.allSatisfy { $0.isPR })
        #expect(prBoard([], reviews: [:], now: now).isEmpty)
    }

    @Test func lanesHaveDistinctLabels() {
        #expect(Set(PRLane.allCases.map(\.title)).count == PRLane.allCases.count)
        #expect(Set(PRLane.allCases.map(\.blurb)).count == PRLane.allCases.count)
        for lane in PRLane.allCases { #expect(!lane.symbolName.isEmpty && lane.id == lane.rawValue) }
    }
}
