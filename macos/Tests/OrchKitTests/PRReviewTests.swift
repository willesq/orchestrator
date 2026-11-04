import Foundation
import Testing
@testable import OrchKit

private let url1 = "https://github.com/willesq/theCalculator/pull/1"
private let url2 = "https://github.com/willesq/theCalculator/pull/2"

@Suite struct PRReviewModelTests {
    @Test func newVerdictsSkipsFirstLoadAndRepeats() {
        let at = Date(timeIntervalSince1970: 1_800_000_000)
        let verdict = PRReview(url: url1, state: .verdict, verdict: .approve, verdictAt: at)
        #expect(newVerdicts(previous: nil, current: [verdict]).isEmpty)
        #expect(newVerdicts(previous: [PRReview(url: url1, state: .reviewing)], current: [verdict]) == [verdict])
        #expect(newVerdicts(previous: [verdict], current: [verdict]).isEmpty)
        let newer = PRReview(url: url1, state: .verdict, verdict: .wait, verdictAt: at.addingTimeInterval(60))
        #expect(newVerdicts(previous: [verdict], current: [newer]) == [newer])
        // A stale verdict on a review that moved on is not news.
        let moved = PRReview(url: url2, state: .reviewing, verdict: .approve, verdictAt: at)
        #expect(newVerdicts(previous: [], current: [moved]).isEmpty)
    }

    @Test func newlyHeldSkipsFirstLoadAndRepeats() {
        let held = PRReview(url: url1, state: .held)
        #expect(newlyHeld(previous: nil, current: [held]).isEmpty)
        #expect(newlyHeld(previous: [PRReview(url: url1, state: .waiting)], current: [held]) == [held])
        #expect(newlyHeld(previous: [held], current: [held]).isEmpty)
    }

    @Test func labelsAndStates() {
        #expect(Set(PRReviewMode.allCases.map(\.explanation)).count == PRReviewMode.allCases.count)
        #expect(PRReviewMode(mayApprove: true, mayComment: true) == .auto)
        #expect(PRReviewMode(mayApprove: true, mayComment: false) == .approve)
        #expect(PRReviewMode(mayApprove: false, mayComment: true) == .feedback)
        #expect(PRReviewMode(mayApprove: false, mayComment: false) == .verdict)
        #expect(PRReviewMode.auto.mayApprove && PRReviewMode.auto.mayComment)
        #expect(!PRReviewMode.comment.mayApprove && !PRReviewMode.comment.mayComment)
        for stage in PRReviewStage.allCases { #expect(!stage.label.isEmpty) }
        #expect(PRReviewStage.waiting < PRReviewStage.posted)
        for state in [PRReviewState.waiting, .held, .reviewing, .verdict, .failed, .closed, .stopped] {
            #expect(!state.label.isEmpty)
        }
        #expect(PRReviewState.waiting.isActive && !PRReviewState.closed.isActive && !PRReviewState.stopped.isActive)
        for v in [PRVerdict.approve, .wait, .changes] {
            #expect(!v.label.isEmpty && !v.symbolName.isEmpty)
        }
        #expect(Set(PRRoleFilter.allCases.map(\.label)).count == PRRoleFilter.allCases.count)
        #expect(Set(PRStateFilter.allCases.map(\.label)).count == PRStateFilter.allCases.count)
    }

    @Test func postedLabelAndStaleness() {
        let sha = String(repeating: "a", count: 40)
        #expect(PRReview(url: url1, headSHA: sha, posted: "approved").postedLabel == "Approved on GitHub at aaaaaaa")
        #expect(PRReview(url: url1, headSHA: sha, posted: "commented").postedLabel == "Comment posted at aaaaaaa")
        #expect(PRReview(url: url1, posted: "failed").postedLabel == "Posting failed")
        #expect(PRReview(url: url1).postedLabel == nil)
        #expect(PRReview(url: url1, state: .waiting, verdict: .approve).verdictIsStale)
        #expect(!PRReview(url: url1, state: .verdict, verdict: .approve).verdictIsStale)
        #expect(!PRReview(url: url1, state: .waiting).verdictIsStale)
    }

    @Test func multilineTextKeepsLineBreaksOnly() {
        #expect(displaySafeLines("a\r\nb\n\nc") == "a\nb\n\nc")
        #expect(displaySafeLines("x\u{202E}y\n\u{0007}") == "x\\u{202E}y\n\\u{0007}")
    }

    @Test func stageFollowsState() {
        #expect(PRReview(url: url1, state: .waiting).stage == .waiting)
        #expect(PRReview(url: url1, state: .reviewing).stage == .reviewing)
        #expect(PRReview(url: url1, state: .failed).stage == .reviewing)
        #expect(PRReview(url: url1, state: .verdict, verdict: .wait).stage == .verdict)
        #expect(PRReview(url: url1, state: .verdict, verdict: .wait, posted: "failed").stage == .verdict)
        #expect(PRReview(url: url1, state: .verdict, verdict: .approve, posted: "approved").stage == .posted)
        #expect(PRReview(url: url1, state: .verdict, verdict: .wait, posted: "commented").stage == .posted)
        #expect(PRReview(url: url1, state: nil).stage == .waiting)
    }

    @Test func checkSummary() {
        let d = PRDetail(url: url1, checks: [
            .init(name: "a", result: "success"), .init(name: "b", result: "skipped"),
            .init(name: "c", result: "failure"), .init(name: "d", result: "timed_out"),
            .init(name: "e", result: "pending"), .init(name: "f", result: "in_progress"),
            .init(name: "g", result: "stale"),
        ])
        let s = d.checkSummary
        #expect(s.passing == 1 && s.skipped == 1 && s.failing == 2 && s.running == 3)
    }

    private func item(_ n: Int, repo: String = "willesq/theCalculator", reasons: [GitHubReason] = [.authored],
                      draft: Bool = false, ci: String = "", attention: Attention? = nil, updated: TimeInterval = 0,
                      isPR: Bool = true, title: String = "Add divide", author: String = "") -> GitHubItem {
        GitHubItem(url: "https://github.com/\(repo)/pull/\(n)", title: title, repo: repo, isPR: isPR, number: n, author: author,
                   updatedAt: Date(timeIntervalSince1970: 1_800_000_000 + updated), isDraft: draft, ciState: ci,
                   reasons: reasons, attention: attention)
    }

    @Test func filtersByRoleStateRepoAndSearch() {
        let mine = item(1, updated: 10)
        let review = item(2, reasons: [.reviewRequested], attention: .reviewRequested)
        let other = item(3, repo: "Org/Api", reasons: [.involved], draft: true, ci: "failure", updated: 30, title: "Fix bug")
        let issue = item(4, isPR: false)
        let all = [mine, review, other, issue]
        let reviews = [mine.url: PRReview(url: mine.url, state: .waiting),
                       review.url: PRReview(url: review.url, state: .stopped, verdict: .wait)]
        func run(_ role: PRRoleFilter = .all, _ state: PRStateFilter = .open, repo: String? = nil, _ search: String = "") -> [Int] {
            filterPRs(all, reviews: reviews, role: role, state: state, repo: repo, author: nil, search: search).map(\.number)
        }
        // Attention first, then newest; issues never.
        #expect(run() == [2, 3, 1])
        #expect(run(.mine) == [1])
        #expect(run(.toReview) == [2])
        #expect(run(.other) == [3])
        #expect(run(.all, .needsAction) == [2])
        #expect(run(.all, .draft) == [3])
        #expect(run(.all, .ciFailing) == [3])
        #expect(run(.all, .watched) == [1])
        #expect(run(.all, .verdicts) == [2])
        #expect(run(repo: "org/api") == [3])
        #expect(run(.all, .open, "fix") == [3])
        #expect(run(.all, .open, "#1") == [1])
        #expect(run(.all, .open, "2") == [2])
        #expect(prRepos(all) == ["Org/Api", "willesq/theCalculator"])
    }

    @Test func filtersByAuthor() {
        let alice = item(1, updated: 10, author: "Alice")
        let bot = item(2, repo: "Org/Api", updated: 20, author: "renovate[bot]")
        let alsoAlice = item(3, repo: "Org/Api", updated: 30, author: "alice")
        let unknown = item(4, updated: 40)
        let issue = item(5, isPR: false, author: "zed")
        let all = [alice, bot, alsoAlice, unknown, issue]
        func run(author: String?, repo: String? = nil, _ search: String = "") -> [Int] {
            filterPRs(all, reviews: [:], role: .all, state: .open, repo: repo, author: author, search: search).map(\.number)
        }
        #expect(run(author: nil) == [4, 3, 2, 1])
        // Case-insensitive and whole-login, so a PR with no author only shows unfiltered.
        #expect(run(author: "ALICE") == [3, 1])
        #expect(run(author: "ali") == [])
        // Combines with the other filters.
        #expect(run(author: "alice", repo: "org/api") == [3])
        #expect(run(author: "renovate[bot]", "add") == [2])
        #expect(run(author: "nobody") == [])
        // One entry per person, none for a blank author, none from issues.
        #expect(prAuthors(all) == ["Alice", "renovate[bot]"])
        #expect(prAuthors([]) == [])
    }
}

private final class FakeReviews: PRReviewAPI, @unchecked Sendable {
    var list: [PRReview] = []
    var detail = PRDetail(url: url1, title: "Add divide")
    var error: Error?
    var watched: [(String, PRReviewMode, String?, String?, Bool)] = []
    var updatesInPlace: [Bool?] = []
    var settings: [Bool] = []
    var stopped: [(String, Bool)] = []
    var detailCalls = 0

    func prReviews() async throws -> [PRReview] {
        if let error { throw error }
        return list
    }

    func watchPRReview(url: String, mode: PRReviewMode, comment: String?, instructions: String?,
                       reviewAnyway: Bool, updateInPlace: Bool?) async throws -> PRReview {
        if let error { throw error }
        updatesInPlace.append(updateInPlace)
        watched.append((url, mode, comment, instructions, reviewAnyway))
        return PRReview(url: url, mode: mode, state: .waiting, comment: comment ?? "", instructions: instructions ?? "",
                        goAhead: reviewAnyway, updateInPlace: updateInPlace ?? false)
    }

    func setPRReviewUpdateInPlace(url: String, on: Bool) async throws -> PRReview {
        if let error { throw error }
        settings.append(on)
        return PRReview(url: url, state: .verdict, updateInPlace: on)
    }

    func stopPRReview(url: String, forget: Bool) async throws {
        if let error { throw error }
        stopped.append((url, forget))
    }

    func prDetail(url: String, refresh: Bool) async throws -> PRDetail {
        detailCalls += 1
        if let error { throw error }
        return detail
    }
}

private struct Boom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    func bump() { lock.withLock { n += 1 } }
    var value: Int { lock.withLock { n } }
}

@MainActor @Suite struct ReviewMonitorTests {
    @Test func announcesOnlyNewVerdicts() async {
        let api = FakeReviews()
        let monitor = ReviewMonitor(api: api, interval: .seconds(3600))
        var announced: [[PRReview]] = []
        monitor.onNewVerdicts = { announced.append($0) }
        let at = Date(timeIntervalSince1970: 1_800_000_000)
        api.list = [PRReview(url: url1, state: .verdict, verdict: .approve, verdictAt: at)]
        await monitor.refresh()
        #expect(announced.isEmpty && monitor.verdictCount == 1)
        var heldNow: [String] = []
        monitor.onNewlyHeld = { heldNow += $0.map(\.url) }
        api.list.append(PRReview(url: url2, state: .verdict, verdict: .changes, verdictAt: at))
        api.list.append(PRReview(url: "https://github.com/a/b/pull/3", state: .held))
        await monitor.refresh()
        #expect(announced.map { $0.map(\.url) } == [[url2]])
        #expect(heldNow == ["https://github.com/a/b/pull/3"])
        #expect(monitor.byURL[url2]?.verdict == .changes)

        api.error = Boom()
        await monitor.refresh()
        #expect(monitor.lastError != nil && monitor.reviews?.count == 3)
    }

    @Test func applyAndRemove() {
        let monitor = ReviewMonitor(api: FakeReviews())
        monitor.apply(PRReview(url: url1, mode: .verdict))
        monitor.apply(PRReview(url: url2))
        monitor.apply(PRReview(url: url1, mode: .approve))
        #expect(monitor.reviews?.map(\.url) == [url2, url1] && monitor.byURL[url1]?.mode == .approve)
        monitor.remove(url2)
        #expect(monitor.reviews?.map(\.url) == [url1])
    }

    @Test func startAndStopPoll() async throws {
        let api = FakeReviews()
        let monitor = ReviewMonitor(api: api, interval: .milliseconds(10))
        monitor.start()
        monitor.start()
        for _ in 0..<100 where monitor.reviews == nil { try await Task.sleep(for: .milliseconds(10)) }
        monitor.stop()
        #expect(monitor.reviews != nil)
    }
}

@MainActor @Suite struct PRsModelTests {
    @Test func watchStopAndErrors() async {
        let api = FakeReviews()
        let model = PRsModel(api: api, reviews: ReviewMonitor(api: api))
        #expect(await model.watch(url1, mode: .comment, comment: "draft"))
        #expect(api.watched.count == 1 && api.watched[0].1 == .comment && api.watched[0].2 == "draft")
        #expect(model.reviews.byURL[url1]?.comment == "draft")

        #expect(await model.stop(url1))
        #expect(api.stopped.count == 1 && !api.stopped[0].1)
        #expect(await model.stop(url1, forget: true))
        #expect(model.reviews.byURL[url1] == nil)

        api.error = Boom()
        #expect(!(await model.watch(url1, mode: .approve)))
        #expect(model.actionErrors[url1] != nil)
        api.error = nil
        #expect(await model.watch(url1, mode: .approve))
        #expect(model.actionErrors[url1] == nil)
    }

    @Test func instructionsCheckNowAndTaskLookup() async {
        let api = FakeReviews()
        let lookups = Counter()
        let model = PRsModel(api: api, reviews: ReviewMonitor(api: api), taskLookup: { id in
            lookups.bump()
            return TaskRecord(id: id, agent: "pr-verdict", body: "", result: "", status: "in_progress", updatedAt: "",
                              run: TaskRunInfo(state: "running"))
        })
        #expect(!(await model.checkNow(url1)))
        #expect(await model.watch(url1, mode: .auto, instructions: "focus on units"))
        #expect(api.watched.last?.3 == "focus on units" && model.reviews.byURL[url1]?.instructions == "focus on units")
        #expect(await model.checkNow(url1))
        #expect(api.watched.last?.1 == .auto && api.watched.last?.3 == nil && api.watched.last?.4 == false)
        #expect(await model.watch(url1, mode: .verdict, reviewAnyway: true))
        #expect(api.watched.last?.4 == true && model.reviews.byURL[url1]?.goAhead == true)
        #expect(await model.watch(url1, mode: .verdict, updateInPlace: true))
        #expect(api.updatesInPlace.last == .some(true) && model.reviews.byURL[url1]?.updateInPlace == true)
        let asked = api.watched.count
        #expect(await model.setUpdateInPlace(url1, on: false))
        #expect(api.settings == [false] && api.watched.count == asked && model.reviews.byURL[url1]?.updateInPlace == false)

        // Not reviewing: no lookup. Reviewing: the task is followed.
        await model.refreshReviewTask(url1)
        #expect(lookups.value == 0)
        model.reviews.apply(PRReview(url: url1, state: .reviewing, taskID: "task-9"))
        await model.refreshReviewTask(url1)
        #expect(lookups.value == 1 && model.reviewTasks["task-9"]?.run?.state == "running")
    }

    @Test func detailLoadsOnceUnlessRefreshed() async {
        let api = FakeReviews()
        let model = PRsModel(api: api, reviews: ReviewMonitor(api: api))
        await model.loadDetail(url1)
        await model.loadDetail(url1)
        #expect(api.detailCalls == 1 && model.details[url1]?.title == "Add divide")
        await model.loadDetail(url1, refresh: true)
        #expect(api.detailCalls == 2)
        api.error = Boom()
        await model.loadDetail(url2)
        #expect(model.detailErrors[url2] != nil && model.details[url2] == nil)
    }

    @Test func selectionAndFilters() async throws {
        let api = FakeReviews()
        let model = PRsModel(api: api, reviews: ReviewMonitor(api: api))
        #expect(!model.isFiltered)
        model.role = .mine
        model.repo = "a/b"
        model.author = "alice"
        model.search = "x"
        #expect(model.isFiltered)
        let items = [GitHubItem(url: url1, title: "x", repo: "a/b", number: 1, author: "alice", reasons: [.authored])]
        #expect(model.visible(items).count == 1)
        model.author = "bob"
        #expect(model.visible(items).isEmpty)
        model.clearFilters()
        #expect(!model.isFiltered)
        #expect(model.author == nil)
        model.author = "alice"
        #expect(model.isFiltered)
        model.clearFilters()
        model.select(url1)
        #expect(model.selection == url1)
        for _ in 0..<100 where model.details[url1] == nil { try await Task.sleep(for: .milliseconds(10)) }
        #expect(model.details[url1] != nil)
        model.select(nil)
        #expect(model.selection == nil)
    }
}

@Suite struct PRReviewRoundTripTests {
    @Test func reviewRPCs() async throws {
        var service = FakeOrchdService()
        var r = Orchd_V1_PRReview()
        r.url = url1
        r.repo = "willesq/theCalculator"
        r.number = 1
        r.mode = "approve"
        r.state = "verdict"
        r.headSha = String(repeating: "a", count: 40)
        r.taskID = "task-1"
        r.verdict = "approve"
        r.reason = "clean"
        r.comment = "LGTM"
        r.posted = "approved"
        r.verdictAt = 1_800_000_000
        r.updatedAt = 1_800_000_100
        r.checkedAt = 1_800_000_050
        r.nextCheckAt = 1_800_000_170
        r.instructions = "notes"
        r.goAhead = true
        r.forUser = "deps fine"
        r.report = "## Review"
        service.prReviews.reviews = [r]
        service.prReview = r
        service.prDetail.url = url1
        service.prDetail.title = "Add divide"
        service.prDetail.createdAt = "2026-10-01T00:00:00Z"
        service.prDetail.additions = 3
        var check = Orchd_V1_PRCheck()
        check.name = "test"
        check.result = "success"
        service.prDetail.checks = [check]
        var file = Orchd_V1_PRFile()
        file.path = "calc.js"
        file.additions = 3
        service.prDetail.files = [file]
        var reviewer = Orchd_V1_PRReviewerState()
        reviewer.login = "bob"
        reviewer.state = "APPROVED"
        service.prDetail.reviews = [reviewer]
        service.prDetail.fetchedAt = 1_800_000_000

        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let list = try await client.prReviews()
            #expect(list.count == 1)
            let got = list[0]
            #expect(got.mode == .approve && got.state == .verdict && got.verdict == .approve && got.number == 1)
            #expect(got.verdictAt == Date(timeIntervalSince1970: 1_800_000_000) && got.posted == "approved")
            #expect(got.checkedAt == Date(timeIntervalSince1970: 1_800_000_050) && got.instructions == "notes" && got.goAhead)
            #expect(got.forUser == "deps fine" && got.report == "## Review")
            #expect(got.nextCheckAt == Date(timeIntervalSince1970: 1_800_000_170))

            _ = try await client.watchPRReview(url: url1, mode: .comment, comment: "edited", instructions: "focus", reviewAnyway: true, updateInPlace: true)
            let sent = try #require(service.recorder.last(Orchd_V1_WatchPRReviewRequest.self))
            #expect(sent.url == url1 && sent.mode == "comment" && sent.setComment && sent.comment == "edited")
            #expect(sent.setInstructions && sent.instructions == "focus" && sent.reviewAnyway)
            #expect(sent.setUpdateInPlace && sent.updateInPlace)
            _ = try await client.watchPRReview(url: url1, mode: .verdict, comment: nil, instructions: nil, reviewAnyway: false, updateInPlace: nil)
            let bare = try #require(service.recorder.last(Orchd_V1_WatchPRReviewRequest.self))
            #expect(!bare.setComment && !bare.setInstructions && !bare.setUpdateInPlace)

            _ = try await client.setPRReviewUpdateInPlace(url: url1, on: true)
            let setting = try #require(service.recorder.last(Orchd_V1_SetPRReviewUpdateInPlaceRequest.self))
            #expect(setting.url == url1 && setting.updateInPlace)

            try await client.stopPRReview(url: url1, forget: true)
            #expect(service.recorder.last(Orchd_V1_StopPRReviewRequest.self)?.forget == true)

            let detail = try await client.prDetail(url: url1, refresh: true)
            #expect(service.recorder.last(Orchd_V1_GetPRDetailRequest.self)?.refresh == true)
            #expect(detail.title == "Add divide" && detail.additions == 3 && detail.createdAt != nil)
            #expect(detail.checks == [.init(name: "test", result: "success")])
            #expect(detail.files == [.init(path: "calc.js", additions: 3, deletions: 0)])
            #expect(detail.reviews == [.init(login: "bob", state: "APPROVED")] && detail.fetchedAt != nil)
        }
    }
}
