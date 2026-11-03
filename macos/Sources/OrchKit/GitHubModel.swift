import Foundation
import Observation

/// Drives the GitHub tab: scopes, their settings, teams and activity.
@MainActor
@Observable
public final class GitHubModel {
    public enum Filter: String, CaseIterable, Identifiable, Sendable {
        case all = "All", prs = "PRs", issues = "Issues"
        public var id: String { rawValue }
    }

    public private(set) var identity: GitHubIdentity?
    public private(set) var scopes: [GitHubScope] = []
    public private(set) var warning: String?
    public private(set) var items: GitHubItemList?
    /// Why scopes or identity could not load; kept until they do, so an
    /// unrelated success cannot hide it.
    public private(set) var scopesError: String?
    /// The last failed activity load or scope change.
    public private(set) var lastError: String?
    /// Owners with a change on its way to orchd.
    public private(set) var saving: Set<String> = []
    /// Repos per owner, for the picker, loaded on demand.
    public private(set) var repos: [String: [GitHubRepo]] = [:]
    /// Why loading an owner's repos failed, shown in the picker.
    public private(set) var repoErrors: [String: String] = [:]
    public private(set) var isRefreshing = false
    /// Whether a check of GitHub the user asked for is still on its way.
    public private(set) var isCheckingGitHub = false
    /// Why the last check of GitHub did not finish, until the next one starts.
    public private(set) var checkWarning: String?
    public var filter = Filter.all

    private let api: any GitHubAPI
    private let interval: Duration
    private let checkPoll: Duration
    private let checkTimeout: Duration
    private var pollTask: Task<Void, Never>?

    /// checkTimeout outlasts orchd's wait between requested sweeps (a minute)
    /// plus the sweep itself.
    public init(api: any GitHubAPI, interval: Duration = .seconds(30),
                checkPoll: Duration = .seconds(2), checkTimeout: Duration = .seconds(120)) {
        self.api = api
        self.interval = interval
        self.checkPoll = checkPoll
        self.checkTimeout = checkTimeout
    }

    /// Items in enabled scopes passing the filter, newest first; items that
    /// need the user come first.
    public var visibleItems: [GitHubItem] {
        let all = items?.items ?? []
        let kept = all.filter { item in
            switch filter {
            case .all: true
            case .prs: item.isPR
            case .issues: !item.isPR
            }
        }
        return kept.sorted { a, b in
            if (a.attention != nil) != (b.attention != nil) { return a.attention != nil }
            return (a.updatedAt ?? .distantPast) > (b.updatedAt ?? .distantPast)
        }
    }

    /// The visible items split into those that need the user and the rest,
    /// each newest first.
    public var sections: (needsAction: [GitHubItem], other: [GitHubItem]) {
        let items = visibleItems
        return (items.filter { $0.attention != nil }, items.filter { $0.attention == nil })
    }

    /// Reloads identity, scopes and items. `rediscover` asks GitHub for the
    /// user's orgs and teams now rather than from orchd's cache.
    public func refresh(rediscover: Bool = false) async {
        guard !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            let list = try await api.gitHubScopes(refresh: rediscover)
            // A scope with a change on its way keeps the local value, so a
            // reload that started before the save cannot undo it on screen.
            scopes = list.scopes.map { fresh in
                saving.contains(fresh.owner) ? (scopes.first { $0.owner == fresh.owner } ?? fresh) : fresh
            }
            warning = list.warning
            identity = try await api.gitHubIdentity(refresh: false)
            scopesError = nil
        } catch {
            // Leaving the tab cancels the poll; the failure that follows is
            // that, not something orchd said.
            if Task.isCancelled { return }
            scopesError = DaemonMonitor.describe(error)
        }
        await refreshItems()
    }

    public func refreshItems() async {
        do {
            items = try await api.gitHubItems()
            lastError = nil
        } catch {
            if Task.isCancelled { return }
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Reloads, asks orchd to search GitHub now, and follows the activity list
    /// until the cache is rewritten. orchd does not report a failed sweep, so
    /// one that never lands ends in checkWarning.
    public func checkGitHub(rediscover: Bool = false) async {
        guard !isCheckingGitHub else { return }
        isCheckingGitHub = true
        defer { isCheckingGitHub = false }
        checkWarning = nil
        await refresh(rediscover: rediscover)
        // refresh does nothing while another is running, and this time has to
        // be the cache's own from before the request.
        await refreshItems()
        let before = items?.updatedAt
        do {
            try await api.requestGitHubSweep()
        } catch {
            if Task.isCancelled { return }
            checkWarning = "Could not ask orchd to check GitHub: \(DaemonMonitor.describe(error))"
            return
        }
        let clock = ContinuousClock()
        let deadline = clock.now + checkTimeout
        while clock.now < deadline {
            try? await Task.sleep(for: checkPoll)
            if Task.isCancelled { return }
            await refreshItems()
            if let updated = items?.updatedAt, before.map({ updated > $0 }) ?? true { return }
        }
        checkWarning = "GitHub has not answered yet, so this may be out of date. It updates here once orchd gets through."
    }

    /// Polls items while the tab is in use; calling it again is a no-op.
    public func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            await self?.refresh()
            while !Task.isCancelled {
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
                // Retry scopes and identity until they load (orchd may have
                // been starting or upgrading); after that only items change
                // on their own. orchd answers these from its identity cache.
                if self?.needsScopes == true {
                    await self?.refresh()
                } else {
                    await self?.refreshItems()
                }
            }
        }
    }

    public func stop() {
        pollTask?.cancel()
        pollTask = nil
    }

    /// Whether scopes or identity still have to load.
    public var needsScopes: Bool { scopes.isEmpty || identity == nil || scopesError != nil }

    @discardableResult
    public func setEnabled(_ owner: String, _ enabled: Bool) async -> Bool {
        await update(owner) { $0.enabled = enabled }
    }

    @discardableResult
    public func setItems(_ owner: String, _ mode: GitHubItemsMode) async -> Bool {
        await update(owner) { $0.items = mode }
    }

    /// Sets which repos count. Picking only some, or all but some, needs at
    /// least one repo; with none it falls back to all repos.
    @discardableResult
    public func setRepos(_ owner: String, mode: GitHubRepoMode, repos: [String]) async -> Bool {
        await update(owner) {
            let picked = Array(Set(repos)).sorted { $0.lowercased() < $1.lowercased() }
            if mode == .all || picked.isEmpty {
                $0.repoMode = .all
                $0.repos = []
            } else {
                $0.repoMode = mode
                $0.repos = picked
            }
        }
    }

    /// Sets the repos watched for every open item; an empty list clears them.
    @discardableResult
    public func setEverythingRepos(_ owner: String, repos: [String]) async -> Bool {
        await update(owner) {
            $0.everythingRepos = Array(Set(repos)).sorted { $0.lowercased() < $1.lowercased() }
        }
    }

    /// Loads the repos the user can see under owner. Every picker that opens
    /// asks again, so a repo made since the last look shows up; the list already
    /// on screen stays while that runs and if it fails.
    public func loadRepos(_ owner: String) async {
        let key = owner.lowercased()
        repoErrors[key] = nil
        do {
            repos[key] = try await api.gitHubRepos(owner: owner)
        } catch {
            // Closing the sheet cancels the load; that is not a failure to show.
            if Task.isCancelled { return }
            repoErrors[key] = DaemonMonitor.describe(error)
        }
    }

    public func reposFor(_ owner: String) -> [GitHubRepo]? { repos[owner.lowercased()] }

    public func repoError(_ owner: String) -> String? { repoErrors[owner.lowercased()] }

    /// Sends a change to one scope; reports whether it is now stored (an
    /// unchanged scope counts as stored).
    private func update(_ owner: String, _ change: (inout GitHubScope) -> Void) async -> Bool {
        guard let index = scopes.firstIndex(where: { $0.owner.caseInsensitiveCompare(owner) == .orderedSame }),
              !saving.contains(scopes[index].owner) else { return false }
        var next = scopes[index]
        change(&next)
        guard next != scopes[index] else { return true }
        let key = next.owner
        saving.insert(key)
        defer { saving.remove(key) }
        do {
            let stored = try await api.setGitHubScope(next)
            if let i = scopes.firstIndex(where: { $0.owner == key }) { scopes[i] = stored }
            lastError = nil
            return true
        } catch {
            lastError = "Could not update \(key): \(DaemonMonitor.describe(error))"
            return false
        }
    }
}
