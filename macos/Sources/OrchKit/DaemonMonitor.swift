import Foundation
import Observation

/// Polls orchd and publishes its health for the tray.
@MainActor
@Observable
public final class DaemonMonitor {
    public private(set) var health: Health = .unknown
    public private(set) var lastChecked: Date?

    private let api: any DaemonAPI
    private let interval: Duration
    private var pollTask: Task<Void, Never>?

    public init(api: any DaemonAPI, interval: Duration = .seconds(10)) {
        self.api = api
        self.interval = interval
    }

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

    /// Checks orchd once, now.
    public func refresh() async {
        do {
            health = Health(status: try await api.status())
        } catch {
            health = .down(reason: Self.describe(error))
        }
        lastChecked = Date()
    }

    /// Keeps the tray text short: gRPC errors carry long transport detail.
    static func describe(_ error: any Error) -> String {
        let text = String(describing: error)
        if text.localizedCaseInsensitiveContains("unavailable") { return "unavailable" }
        if text.localizedCaseInsensitiveContains("deadline") { return "timed out" }
        // An orchd older than this app lacks the RPC.
        if text.localizedCaseInsensitiveContains("unimplemented") {
            return "needs a newer orchd; update orch (brew upgrade orch)"
        }
        return text.count > 80 ? String(text.prefix(80)) + "…" : text
    }
}
