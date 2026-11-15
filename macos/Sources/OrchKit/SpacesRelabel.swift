import Foundation
import Observation

/// What one Relabel call found and did (SP-027). Counts only: orchd's
/// per-space transitions are left out, since the pane shows none of them.
public struct RelabelRun: Sendable, Equatable {
    /// The applied policy revision chunks were brought up to.
    public var revision: Int64
    /// Chunks in every collection.
    public var total: Int64
    /// Chunks not labelled by `revision` when the run read them.
    public var stale: Int64
    /// Chunks stamped or moved (or, for a count, that would be).
    public var changed: Int64
    /// Of those, chunks that changed collection.
    public var moved: Int64
    /// Stale chunks left stale, for the next run.
    public var failed: Int64
    /// Set when the run stamped and moved, not just counted.
    public var applied: Bool
    /// Why an applied run stopped early; what it did is still counted.
    public var error: String

    public init(revision: Int64 = 0, total: Int64 = 0, stale: Int64 = 0, changed: Int64 = 0, moved: Int64 = 0,
                failed: Int64 = 0, applied: Bool = false, error: String = "") {
        self.revision = revision
        self.total = total
        self.stale = stale
        self.changed = changed
        self.moved = moved
        self.failed = failed
        self.applied = applied
        self.error = error
    }
}

/// orchd's Relabel RPC, operator-only: under a policy only Orch.app may
/// call it (D-71).
public protocol SpacesRelabelAPI: Sendable {
    /// Relabels every stale chunk, or with `apply` false only counts them.
    func relabel(apply: Bool) async throws -> RelabelRun
}

/// Drives the Spaces pane's "Relabel now" control (SP-079, D-76): counts
/// the chunks not labelled by the applied revision, runs a relabel when the
/// user asks, and reports how it went. orchd never relabels on its own
/// after an apply, and neither does this model: a refused or failed run is
/// shown and left for the user to start again.
@MainActor
@Observable
public final class SpacesRelabelModel {
    public enum Phase: Sendable, Equatable {
        /// Not counted yet, or the count failed (`checkError`).
        case unknown
        /// Every chunk is labelled by the applied revision.
        case upToDate
        /// This many chunks are stale.
        case stale(Int64)
        /// A relabel of this many stale chunks is under way.
        case running(Int64)
    }

    public private(set) var phase: Phase = .unknown
    /// The applied revision the last count or run reported.
    public private(set) var revision: Int64 = 0
    /// The last run this session started, kept to say what it did.
    public private(set) var lastRun: RelabelRun?
    /// Why the last run was refused or stopped early, or left chunks stale.
    public private(set) var failure: String?
    /// Why the last count failed.
    public private(set) var checkError: String?
    public private(set) var isChecking = false

    /// Called when a run starts and ends, so the Overview and the sidebar
    /// badge pick up the run's progress at once.
    public var onChange: (@MainActor () async -> Void)?

    private let api: any SpacesRelabelAPI
    private var generation = 0

    public init(api: any SpacesRelabelAPI) {
        self.api = api
    }

    public var isRunning: Bool {
        if case .running = phase { return true }
        return false
    }

    /// Whether "Relabel now" can be pressed: something is stale, no run is
    /// under way, and no count is, so a run's follow-up count must answer
    /// before another can start.
    public var canStart: Bool {
        if case .stale(let n) = phase { return n > 0 && !isChecking }
        return false
    }

    /// Whether the pane shows the control: with nothing stale, nothing
    /// running and nothing to report from this session, it is absent.
    public var isVisible: Bool {
        switch phase {
        case .stale, .running: true
        case .unknown: checkError != nil || failure != nil
        case .upToDate: lastRun != nil || failure != nil
        }
    }

    /// The run's progress for the Overview's needs-you strip and the sidebar
    /// badge. Relabel answers only when it is done, so a run under way
    /// reports none of its chunks done yet.
    public var progress: RelabelProgress? {
        if case .running(let n) = phase { return RelabelProgress(done: 0, total: n) }
        return nil
    }

    /// Counts the stale chunks without changing any. Skipped while a run is
    /// under way: orchd holds its migration lock for the run and would
    /// refuse the count.
    public func check() async {
        guard !isRunning else { return }
        generation += 1
        let mine = generation
        isChecking = true
        defer { if mine == generation { isChecking = false } }
        do {
            let count = try await api.relabel(apply: false)
            // A run started while this count was in flight wins.
            guard mine == generation, !isRunning else { return }
            revision = count.revision
            phase = count.stale > 0 ? .stale(count.stale) : .upToDate
            checkError = nil
        } catch {
            // Leaving the pane cancels its count; that is not a failure.
            guard mine == generation, !isRunning, !Task.isCancelled else { return }
            phase = .unknown
            checkError = spacesRefusal(error)
        }
    }

    /// Relabels every stale chunk, once. Afterwards it counts again, so the
    /// control shows what is still stale; it never starts another run.
    public func start() async {
        guard canStart, case .stale(let n) = phase else { return }
        generation += 1
        phase = .running(n)
        failure = nil
        await onChange?()
        do {
            let run = try await api.relabel(apply: true)
            lastRun = run
            revision = run.revision
            if !run.error.isEmpty {
                failure = displaySafe(run.error)
            } else if run.failed > 0 {
                failure = run.failed == 1
                    ? "1 chunk could not be placed and is still stale. Relabel again later; orchd's log says why."
                    : "\(run.failed) chunks could not be placed and are still stale. Relabel again later; orchd's log says why."
            }
        } catch {
            failure = spacesRefusal(error)
        }
        // The button stays disabled until this count answers (canStart).
        phase = .stale(n)
        await check()
        await onChange?()
    }
}
