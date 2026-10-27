import Foundation
import Observation

/// One point of a process's history, for the sparklines.
public struct MetricSample: Sendable, Equatable, Identifiable {
    public var id: Date { at }
    public let at: Date
    public let cpuPercent: Double
    public let rssBytes: UInt64
}

/// Drives the System tab's overview: live process metrics with a short
/// history, subsystems with their heartbeats, and the full status report.
@MainActor
@Observable
public final class SystemModel {
    /// Samples kept per process: six minutes while the tab is shown, longer
    /// from the slower sampling while it is not.
    public static let historyLength = 180

    public private(set) var metrics: SystemMetrics?
    public private(set) var status: DaemonStatus?
    public private(set) var history: [String: [MetricSample]] = [:]
    public private(set) var report: StatusReport?
    /// Why the last metrics or status poll failed; cleared by the next success.
    public private(set) var lastError: String?
    public private(set) var reportError: String?
    public private(set) var loadingReport = false
    private var pids: [String: Int] = [:]

    private let api: any SystemAPI
    private let loadReport: @Sendable () async throws -> StatusReport
    private let interval: Duration
    private let reportInterval: Duration
    private let backgroundInterval: Duration
    private var pollTask: Task<Void, Never>?
    private var reportTask: Task<Void, Never>?
    private var backgroundTask: Task<Void, Never>?

    public init(api: any SystemAPI, loadReport: @escaping @Sendable () async throws -> StatusReport,
                interval: Duration = .seconds(2), reportInterval: Duration = .seconds(15),
                backgroundInterval: Duration = .seconds(10)) {
        self.api = api
        self.loadReport = loadReport
        self.interval = interval
        self.reportInterval = reportInterval
        self.backgroundInterval = backgroundInterval
    }

    /// Records one metrics sample without touching status, for the slow
    /// sampling that keeps the sparklines' history while the tab is hidden.
    public func sample() async {
        guard let m = try? await api.systemMetrics() else { return }
        record(m)
    }

    /// Samples metrics slowly for as long as the app runs, skipping while the
    /// tab polls, so the sparklines open with history instead of two points.
    /// Calling it again is a no-op.
    public func startSampling() {
        guard backgroundTask == nil else { return }
        backgroundTask = Task { [weak self] in
            while !Task.isCancelled {
                if self?.pollTask == nil { await self?.sample() }
                guard let interval = self?.backgroundInterval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stopSampling() {
        backgroundTask?.cancel()
        backgroundTask = nil
    }

    public func refresh() async {
        do {
            async let m = api.systemMetrics()
            async let s = api.status()
            let (metrics, status) = try await (m, s)
            record(metrics)
            self.status = status
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    public func refreshReport() async {
        guard !loadingReport else { return }
        loadingReport = true
        defer { loadingReport = false }
        do {
            report = try await loadReport()
            reportError = nil
        } catch {
            reportError = error.localizedDescription
        }
    }

    /// Polls while the tab is in use; calling it again is a no-op.
    public func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refresh()
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
            }
        }
        // `orch status` probes everything, so it runs far less often.
        reportTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refreshReport()
                guard let interval = self?.reportInterval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        pollTask?.cancel()
        reportTask?.cancel()
        pollTask = nil
        reportTask = nil
    }

    private func record(_ m: SystemMetrics) {
        metrics = m
        for p in m.processes {
            var samples = history[p.name] ?? []
            // A process that stopped, or restarted under a new PID, starts a
            // new line rather than joining the old one.
            if !p.running {
                history[p.name] = []
                pids[p.name] = nil
                continue
            }
            if let pid = pids[p.name], pid != p.pid { samples = [] }
            pids[p.name] = p.pid
            samples.append(MetricSample(at: m.at, cpuPercent: p.cpuPercent, rssBytes: p.rssBytes))
            if samples.count > Self.historyLength { samples.removeFirst(samples.count - Self.historyLength) }
            history[p.name] = samples
        }
    }
}

/// Drives the System tab's live logs: polls orchd for new lines, keeps the
/// most recent locally, and filters by source and text.
@MainActor
@Observable
public final class LogsModel {
    /// Lines kept locally, matching what orchd keeps.
    public static let capacity = 5000

    public private(set) var lines: [LogLine] = []
    public private(set) var sources: [String] = []
    /// Lines that rolled out of orchd's memory before the app read them.
    public private(set) var missed: UInt64 = 0
    public private(set) var lastError: String?
    /// nil shows every source.
    public var source: String?
    public var filter = ""
    /// While paused nothing new is fetched; resuming catches up.
    public var paused = false

    private let api: any SystemAPI
    private let interval: Duration
    private var lastSeq: UInt64 = 0
    private var polling = false
    private var pollTask: Task<Void, Never>?

    public init(api: any SystemAPI, interval: Duration = .seconds(1)) {
        self.api = api
        self.interval = interval
    }

    public var visibleLines: [LogLine] {
        let needle = filter.trimmingCharacters(in: .whitespaces)
        guard source != nil || !needle.isEmpty else { return lines }
        return lines.filter { line in
            (source == nil || line.source == source)
                && (needle.isEmpty || line.message.range(of: needle, options: .caseInsensitive) != nil)
        }
    }

    public func poll() async {
        // One call at a time: a restarted loop, or a quick pause and resume,
        // must not ask twice for the same lines.
        guard !paused, !polling else { return }
        polling = true
        defer { polling = false }
        do {
            let page = try await api.tailLogs(after: lastSeq, limit: lastSeq == 0 ? 1000 : Self.capacity)
            // Paused while the call was out: drop the page so resuming fetches
            // from where the view stopped.
            guard !paused else { return }
            apply(page)
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    func apply(_ page: LogPage) {
        // A lower sequence means orchd restarted and numbering began again.
        // The old lines go: their sequence numbers, the lines' identities,
        // would collide with the new process's.
        if page.lastSeq < lastSeq {
            lines.removeAll()
            lastSeq = 0
            missed = 0
        }
        // Only lines not already shown, so line identities stay unique.
        let fresh = page.lines.filter { $0.seq > lastSeq }
        // A gap before the first new line is lines orchd no longer kept, or
        // more than one reply carries.
        if lastSeq > 0, let first = fresh.first, first.seq > lastSeq + 1 {
            missed += first.seq - lastSeq - 1
        }
        lines.append(contentsOf: fresh)
        if lines.count > Self.capacity { lines.removeFirst(lines.count - Self.capacity) }
        lastSeq = max(lastSeq, page.lastSeq)
        sources = page.sources
    }

    /// Clears the lines shown; new ones keep arriving.
    public func clear() {
        lines.removeAll()
        missed = 0
    }

    public func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        pollTask?.cancel()
        pollTask = nil
    }
}
