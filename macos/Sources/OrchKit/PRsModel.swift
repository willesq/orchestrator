import Foundation
import Observation

/// Polls orchd's PR review watches for the whole app, so a verdict is
/// announced whether or not the GitHub tab is open. `ListPRReviews` reads only
/// orch.db, so polling it is cheap.
@MainActor
@Observable
public final class ReviewMonitor {
    public private(set) var reviews: [PRReview]?
    public private(set) var lastError: String?

    /// Called with reviews that have a new verdict, after each refresh.
    public var onNewVerdicts: (@MainActor ([PRReview]) -> Void)?
    /// Called with reviews newly held for the user's go-ahead.
    public var onNewlyHeld: (@MainActor ([PRReview]) -> Void)?

    private let api: any PRReviewAPI
    private let interval: Duration
    private var pollTask: Task<Void, Never>?

    public init(api: any PRReviewAPI, interval: Duration = .seconds(20)) {
        self.api = api
        self.interval = interval
    }

    /// Reviews by PR URL.
    public var byURL: [String: PRReview] {
        Dictionary((reviews ?? []).map { ($0.url, $0) }, uniquingKeysWith: { a, _ in a })
    }

    /// Verdicts the user has not acted on: active reviews with a verdict for
    /// the current head.
    public var verdictCount: Int { (reviews ?? []).filter { $0.state == .verdict }.count }

    public func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refresh()
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        pollTask?.cancel()
        pollTask = nil
    }

    public func refresh() async {
        do {
            let next = try await api.prReviews()
            let fresh = newVerdicts(previous: reviews, current: next)
            let held = newlyHeld(previous: reviews, current: next)
            reviews = next
            lastError = nil
            if !fresh.isEmpty { onNewVerdicts?(fresh) }
            if !held.isEmpty { onNewlyHeld?(held) }
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Records a review orchd just returned, ahead of the next poll. A verdict
    /// it carries counts as seen, so the user's own change is not announced.
    public func apply(_ review: PRReview) {
        var list = reviews ?? []
        if let i = list.firstIndex(where: { $0.url == review.url }) {
            list[i] = review
        } else {
            list.insert(review, at: 0)
        }
        reviews = list
    }

    public func remove(_ url: String) {
        reviews?.removeAll { $0.url == url }
    }
}

/// Drives the GitHub tab's PRs pane: filters, the selected PR, its detail, and
/// review watch changes.
@MainActor
@Observable
public final class PRsModel {
    public var role = PRRoleFilter.all
    public var state = PRStateFilter.open
    /// Lists only the PRs orchd is still watching for review.
    public var watchedOnly = false
    /// nil lists every repo.
    public var repo: String?
    /// nil lists every author.
    public var author: String?
    public var search = ""
    public private(set) var selection: String?

    public private(set) var details: [String: PRDetail] = [:]
    public private(set) var detailErrors: [String: String] = [:]
    public private(set) var loadingDetail: Set<String> = []
    /// PRs with a review change on its way to orchd.
    public private(set) var saving: Set<String> = []
    /// The last failed review change, per PR.
    public private(set) var actionErrors: [String: String] = [:]
    /// Review tasks by ID, for how a review under way is going.
    public private(set) var reviewTasks: [String: TaskRecord] = [:]

    public let reviews: ReviewMonitor
    private let api: any PRReviewAPI
    private let taskLookup: (@Sendable (String) async throws -> TaskRecord?)?

    public init(api: any PRReviewAPI, reviews: ReviewMonitor,
                taskLookup: (@Sendable (String) async throws -> TaskRecord?)? = nil) {
        self.api = api
        self.reviews = reviews
        self.taskLookup = taskLookup
    }

    /// Reloads the review task of url's review, if one is under way; a failed
    /// lookup keeps the last one.
    public func refreshReviewTask(_ url: String) async {
        guard let review = reviews.byURL[url], review.state == .reviewing, !review.taskID.isEmpty,
              let taskLookup else { return }
        if let task = try? await taskLookup(review.taskID) {
            reviewTasks[review.taskID] = task
        }
    }

    /// Asks orchd to read url's PR from GitHub now, keeping what it may post.
    @discardableResult
    public func checkNow(_ url: String) async -> Bool {
        guard let review = reviews.byURL[url] else { return false }
        let ok = await watch(url, mode: review.mode)
        // orchd checks within a tick; pick the result up soon after.
        Task {
            try? await Task.sleep(for: .seconds(18))
            await reviews.refresh()
        }
        return ok
    }

    /// The PRs in items the filters keep.
    public func visible(_ items: [GitHubItem]) -> [GitHubItem] {
        filterPRs(items, reviews: reviews.byURL, role: role, state: state, watchedOnly: watchedOnly, repo: repo,
                  author: author, search: search)
    }

    /// Whether any filter narrows the list.
    public var isFiltered: Bool { role != .all || state != .open || watchedOnly || repo != nil || author != nil || !search.isEmpty }

    public func clearFilters() {
        role = .all
        state = .open
        watchedOnly = false
        repo = nil
        author = nil
        search = ""
    }

    /// Selects a PR and loads its detail; nil clears the selection.
    public func select(_ url: String?) {
        selection = url
        guard let url else { return }
        Task { await loadDetail(url) }
    }

    /// Loads url's detail unless it is loaded already or refresh is set.
    public func loadDetail(_ url: String, refresh: Bool = false) async {
        guard refresh || details[url] == nil, !loadingDetail.contains(url) else { return }
        loadingDetail.insert(url)
        defer { loadingDetail.remove(url) }
        do {
            details[url] = try await api.prDetail(url: url, refresh: refresh)
            detailErrors[url] = nil
        } catch {
            detailErrors[url] = DaemonMonitor.describe(error)
        }
    }

    /// Starts watching url for review or changes what orchd may post; a
    /// non-nil comment replaces the draft. Reports whether orchd stored it.
    @discardableResult
    public func watch(_ url: String, mode: PRReviewMode, comment: String? = nil, instructions: String? = nil,
                      reviewAnyway: Bool = false, updateInPlace: Bool? = nil) async -> Bool {
        await change(url) {
            let review = try await self.api.watchPRReview(url: url, mode: mode, comment: comment, instructions: instructions,
                                                          reviewAnyway: reviewAnyway, updateInPlace: updateInPlace)
            self.reviews.apply(review)
        }
    }

    /// Changes only whether later comments edit orch's earlier one on url's PR.
    @discardableResult
    public func setUpdateInPlace(_ url: String, on: Bool) async -> Bool {
        await change(url) {
            self.reviews.apply(try await self.api.setPRReviewUpdateInPlace(url: url, on: on))
        }
    }

    /// Stops watching url; forget also deletes its verdict.
    @discardableResult
    public func stop(_ url: String, forget: Bool = false) async -> Bool {
        await change(url) {
            try await self.api.stopPRReview(url: url, forget: forget)
            if forget {
                self.reviews.remove(url)
            } else {
                await self.reviews.refresh()
            }
        }
    }

    private func change(_ url: String, _ work: () async throws -> Void) async -> Bool {
        guard !saving.contains(url) else { return false }
        saving.insert(url)
        defer { saving.remove(url) }
        do {
            try await work()
            actionErrors[url] = nil
            return true
        } catch {
            actionErrors[url] = DaemonMonitor.describe(error)
            return false
        }
    }
}
