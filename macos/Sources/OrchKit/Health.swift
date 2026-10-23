import Foundation

/// One supervised orchd unit, as `orch status` reports it.
public struct Subsystem: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    /// `running`, `stopped` or `failed`.
    public let state: String
    public let detail: String
    /// The last start or stop failure; empty when healthy.
    public let error: String
    public let restarts: Int

    public init(name: String, state: String, detail: String = "", error: String = "", restarts: Int = 0) {
        self.name = name
        self.state = state
        self.detail = detail
        self.error = error
        self.restarts = restarts
    }

    public var isFailed: Bool { state == "failed" || !error.isEmpty }
}

/// A snapshot of orchd from its `Status` RPC.
public struct DaemonStatus: Sendable, Equatable {
    public let version: String
    public let uptimeSeconds: Int64
    public let subsystems: [Subsystem]

    public init(version: String, uptimeSeconds: Int64, subsystems: [Subsystem]) {
        self.version = version
        self.uptimeSeconds = uptimeSeconds
        self.subsystems = subsystems
    }
}

/// What the tray icon shows.
public enum Health: Sendable, Equatable {
    case unknown
    case healthy(DaemonStatus)
    case degraded(DaemonStatus, failed: [String])
    case down(reason: String)

    /// Classifies a status: any failed unit makes the daemon degraded.
    /// `stopped` is deliberate (a unit someone stopped) and does not count.
    public init(status: DaemonStatus) {
        let failed = status.subsystems.filter(\.isFailed).map(\.name)
        self = failed.isEmpty ? .healthy(status) : .degraded(status, failed: failed)
    }

    public var status: DaemonStatus? {
        switch self {
        case .healthy(let s), .degraded(let s, _): s
        case .unknown, .down: nil
        }
    }

    /// An SF Symbol for the menu bar.
    public var symbolName: String {
        switch self {
        case .unknown: "circle.dotted"
        case .healthy: "brain"
        case .degraded: "exclamationmark.triangle"
        case .down: "bolt.slash"
        }
    }

    public var summary: String {
        switch self {
        case .unknown: "Checking orchd…"
        case .healthy(let s): "orchd \(s.version) running, up \(formatUptime(s.uptimeSeconds))"
        case .degraded(_, let failed): "Degraded: \(failed.joined(separator: ", ")) failed"
        case .down(let reason): "orchd is not reachable (\(reason))"
        }
    }
}

/// Formats seconds the way `orch status` does: `1h31m`, `5m`, `42s`.
public func formatUptime(_ seconds: Int64) -> String {
    let s = max(seconds, 0)
    let days = s / 86_400, hours = (s % 86_400) / 3_600, minutes = (s % 3_600) / 60
    if days > 0 { return "\(days)d\(hours)h" }
    if hours > 0 { return "\(hours)h\(minutes)m" }
    if minutes > 0 { return "\(minutes)m" }
    return "\(s)s"
}
