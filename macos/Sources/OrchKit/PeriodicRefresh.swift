import Foundation

/// Runs some reloads now and then every `interval` until stopped. The
/// sidebar uses it to keep its badges current from the models the tabs
/// already load, at a pace far slower than a tab's own polling.
@MainActor
public final class PeriodicRefresh {
    private let interval: Duration
    private let actions: [@MainActor () async -> Void]
    private var task: Task<Void, Never>?

    public init(interval: Duration, actions: [@MainActor () async -> Void]) {
        self.interval = interval
        self.actions = actions
    }

    public var isRunning: Bool { task != nil }

    /// Starts the loop; calling it again is a no-op.
    public func start() {
        guard task == nil else { return }
        task = Task { [weak self] in
            while !Task.isCancelled {
                await self?.runOnce()
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        task?.cancel()
        task = nil
    }

    /// Runs every action once, in order.
    public func runOnce() async {
        for action in actions { await action() }
    }
}
