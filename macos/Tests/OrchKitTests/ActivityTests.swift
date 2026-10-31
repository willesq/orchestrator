import Foundation
import Testing
@testable import OrchKit

// The Dashboard timeline: what becomes an entry, how it reads, and its order.

@Suite struct ActivityFeedTests {
    private let base = Date(timeIntervalSince1970: 1_793_000_000)

    @Test func parsesSQLiteUTCAndISOTaskTimes() throws {
        let sqlite = try #require(ActivityFeed.taskDate("2026-10-31 00:21:36"))
        let iso = try #require(ActivityFeed.taskDate("2026-10-31T00:21:36Z"))
        #expect(sqlite == iso)
        #expect(ActivityFeed.taskDate("") == nil)
        #expect(ActivityFeed.taskDate("yesterday") == nil)
    }

    @Test(arguments: [
        ("complete", ActivityOutcome.done, "finished"),
        ("failed", .failed, "failed"),
        ("in_progress", .running, "running"),
        ("pending", .waiting, "queued"),
        ("blocked", .attention, "blocked"),
        ("interrupted", .attention, "interrupted"),
        ("cancelled", .waiting, "cancelled"),
        ("mystery", .waiting, "mystery"),
    ])
    func taskStatusMapsToOutcome(status: String, outcome: ActivityOutcome, verb: String) throws {
        let task = TaskRecord(id: "t1", agent: "dev", body: "Fix it", status: status, updatedAt: "2026-10-31 00:21:36")
        let event = try #require(ActivityFeed.taskEvent(task))
        #expect(event.outcome == outcome)
        #expect(event.detail.hasPrefix("dev \(verb)"))
        #expect(event.target == .task("t1"))
    }

    @Test func taskDetailCarriesDurationCostAndPRs() throws {
        let task = TaskRecord(id: "t1", body: "\n\n  Ship the timeline  \nmore", status: "complete",
                              updatedAt: "2026-10-31 00:21:36", costUSD: 1.234, durationMS: 125_000,
                              prs: ["https://github.com/a/b/pull/1"])
        let event = try #require(ActivityFeed.taskEvent(task))
        #expect(event.title == "Ship the timeline")
        #expect(event.detail == "dev finished · 2m 5s · $1.23 · 1 PR")
    }

    @Test func taskWithoutTimeOrBodyIsHandled() throws {
        #expect(ActivityFeed.taskEvent(TaskRecord(id: "t1", updatedAt: "")) == nil)
        let event = try #require(ActivityFeed.taskEvent(TaskRecord(id: "t9", body: "   ", updatedAt: "2026-10-31 00:00:00")))
        #expect(event.title == "t9")
    }

    @Test func headlineCutsLongLines() {
        let long = String(repeating: "x", count: 200)
        let line = ActivityFeed.headline(long, fallback: "f")
        #expect(line.count == 90)
        #expect(line.hasSuffix("…"))
    }

    @Test func durationFormats() {
        #expect(ActivityFeed.formatDuration(ms: 4_000) == "4s")
        #expect(ActivityFeed.formatDuration(ms: 61_000) == "1m 1s")
        #expect(ActivityFeed.formatDuration(ms: 3_720_000) == "1h 2m")
    }

    @Test func reviewVerdictBecomesEntry() throws {
        let review = PRReview(url: "https://github.com/a/b/pull/7", repo: "a/b", number: 7, state: .verdict,
                              verdict: .changes, posted: "commented", verdictAt: base)
        let event = try #require(ActivityFeed.reviewEvent(review))
        #expect(event.title == "a/b#7")
        #expect(event.detail == "Verdict: Changes needed · posted")
        #expect(event.outcome == .attention)
        #expect(event.date == base)
        #expect(event.target == .pr("https://github.com/a/b/pull/7"))
    }

    @Test func failedPostingIsMarkedFailed() throws {
        let review = PRReview(url: "u", repo: "a/b", number: 1, verdict: .approve, posted: "failed", verdictAt: base)
        let event = try #require(ActivityFeed.reviewEvent(review))
        #expect(event.outcome == .failed)
        #expect(event.detail.contains("posting failed"))
    }

    @Test func reviewWithoutVerdictShowsItsStateUnlessInactive() throws {
        let reviewing = PRReview(url: "u", repo: "a/b", number: 1, state: .reviewing, updatedAt: base)
        #expect(try #require(ActivityFeed.reviewEvent(reviewing)).outcome == .running)
        let held = PRReview(url: "u", repo: "a/b", number: 1, state: .held, updatedAt: base)
        #expect(try #require(ActivityFeed.reviewEvent(held)).outcome == .attention)
        #expect(ActivityFeed.reviewEvent(PRReview(url: "u", state: .stopped, updatedAt: base)) == nil)
        #expect(ActivityFeed.reviewEvent(PRReview(url: "u", state: .closed, updatedAt: base)) == nil)
        #expect(ActivityFeed.reviewEvent(PRReview(url: "u", state: .waiting)) == nil)
    }

    @Test func docsSyncIsOneEntryPlusOnePerFailingRepo() {
        let catalog = DocCatalog(
            owners: [DocOwner(owner: "willesq", repos: [
                DocRepo(repo: "ok", lastSynced: base, status: "ok"),
                DocRepo(repo: "bad", lastSynced: base, status: "error", error: "clone failed\nstack"),
                DocRepo(repo: "never", status: "error"),
            ])],
            files: [],
            sync: DocsSyncState(owner: "willesq", finishedAt: base.addingTimeInterval(10))
        )
        let events = ActivityFeed.docsEvents(catalog)
        #expect(events.map(\.id) == ["docs:sync", "docs:willesq/bad", "docs:willesq/never"])
        #expect(events[0].detail == "Synced willesq · 2 repos failed")
        #expect(events[0].outcome == .attention)
        #expect(events[1].detail == "clone failed")
        #expect(events[2].detail == "Docs sync failed")
        // A repo that never synced is dated by the sync that failed it.
        #expect(events[2].date == base.addingTimeInterval(10))
    }

    @Test func runningDocsSyncShowsProgress() throws {
        let catalog = DocCatalog(owners: [], files: [], sync: DocsSyncState(running: true, startedAt: base, done: 3, total: 9))
        let event = try #require(ActivityFeed.docsEvents(catalog).first)
        #expect(event.outcome == .running)
        #expect(event.detail == "Syncing all owners · 3/9")
    }

    @Test(arguments: [("ok", ActivityOutcome.done), ("error", .failed), ("missing", .attention), ("new", .done)])
    func sourceStatusMapsToOutcome(status: String, outcome: ActivityOutcome) throws {
        let source = RegisteredSource(id: "s1", kind: .web, uri: "https://example.com", lastFetched: base, status: status)
        let event = try #require(ActivityFeed.sourceEvent(source))
        #expect(event.outcome == outcome)
        #expect(event.target == .source("s1"))
        #expect(ActivityFeed.sourceEvent(RegisteredSource(id: "s2", kind: .web, uri: "x")) == nil)
    }

    @Test func eventsAreNewestFirstStableAndLimited() {
        let tasks = [
            TaskRecord(id: "old", updatedAt: "2026-10-30 10:00:00"),
            TaskRecord(id: "new", updatedAt: "2026-10-31 10:00:00"),
            TaskRecord(id: "tieB", updatedAt: "2026-10-31 09:00:00"),
            TaskRecord(id: "tieA", updatedAt: "2026-10-31 09:00:00"),
        ]
        let events = ActivityFeed.events(tasks: tasks, reviews: [], docs: nil, sources: [])
        #expect(events.map(\.id) == ["task:new", "task:tieA", "task:tieB", "task:old"])
        #expect(ActivityFeed.events(tasks: tasks, reviews: [], docs: nil, sources: [], limit: 2).count == 2)
        #expect(ActivityFeed.events(tasks: tasks, reviews: [], docs: nil, sources: [], limit: -1).isEmpty)
    }

    @Test func dayHeadings() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = try #require(TimeZone(identifier: "UTC"))
        let now = try #require(ActivityFeed.taskDate("2026-10-31 12:00:00"))
        #expect(ActivityFeed.day(now.addingTimeInterval(-3600), now: now, calendar: calendar) == "Today")
        #expect(ActivityFeed.day(now.addingTimeInterval(-86_400), now: now, calendar: calendar) == "Yesterday")
        #expect(ActivityFeed.day(now.addingTimeInterval(-3 * 86_400), now: now, calendar: calendar) != "Yesterday")
    }
}
