import Foundation
import Observation

/// Polls orchd's PR cache for the tray's "Needs you" list.
@MainActor
@Observable
public final class PRMonitor {
    public private(set) var list: PRList?
    /// The last failure, cleared by the next success. The list keeps its last
    /// good value meanwhile.
    public private(set) var lastError: String?

    /// Called with PRs that newly need the user, after each refresh.
    public var onNewlyNeedingYou: (@MainActor ([PullRequest]) -> Void)?

    private let api: any DaemonAPI
    private let interval: Duration
    private var pollTask: Task<Void, Never>?

    public init(api: any DaemonAPI, interval: Duration = .seconds(60)) {
        self.api = api
        self.interval = interval
    }

    public var needsYou: [PullRequest] { list?.needsYou ?? [] }

    /// Starts polling; calling it again while polling is a no-op.
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

    /// Fetches the list once, now.
    public func refresh() async {
        do {
            let next = try await api.listPRs()
            let previous = list?.needsYou
            list = next
            lastError = nil
            let fresh = newlyNeedingYou(previous: previous, current: next.needsYou)
            if !fresh.isEmpty { onNewlyNeedingYou?(fresh) }
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }
}
