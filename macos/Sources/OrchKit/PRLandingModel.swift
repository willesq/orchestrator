import Foundation
import Observation

/// Loads and acts on what agents proposed for PRs, and follows what their
/// sessions are doing. Every push is the user's click; nothing here retries one.
@MainActor
@Observable
public final class PRLandingModel {
    /// How often a running session's activity and the decisions are read again.
    public static let activityInterval: Duration = .seconds(3)
    public static let decisionInterval: Duration = .seconds(5)

    public private(set) var landings: [String: PRLanding] = [:]
    public private(set) var decisions: [String: PRLanding.State] = [:]
    public private(set) var loading: Set<String> = []
    /// The task an action is in flight for. One at a time, so a double click
    /// cannot push twice.
    public private(set) var acting: String?
    /// The last problem loading or acting on a task's proposal.
    public private(set) var errors: [String: String] = [:]
    public private(set) var activity: [String: TaskActivityLog] = [:]
    public private(set) var activityErrors: [String: String] = [:]

    /// The commits whose diff the user has opened. Push waits for it.
    public private(set) var reviewed: Set<String> = []

    private var generations: [String: Int] = [:]
    private let api: any PRLandingAPI
    private let sleep: @Sendable (Duration) async throws -> Void

    public init(api: any PRLandingAPI, sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }) {
        self.api = api
        self.sleep = sleep
    }

    /// Reads the decisions until cancelled.
    public func follow() async {
        while !Task.isCancelled {
            await refreshDecisions()
            do { try await sleep(Self.decisionInterval) } catch { break }
        }
    }

    public func refreshDecisions() async {
        if let fresh = try? await api.landingDecisions() { decisions = fresh }
    }

    /// Whether task has a proposal the user has not decided about yet.
    public func awaitsDecision(_ task: TaskRecord) -> Bool {
        PRHandoff.proposes(task) && decisions[task.id] == nil
    }

    /// The newest of tasks with a proposal waiting on the user.
    public func awaiting(in tasks: [TaskRecord]) -> TaskRecord? {
        tasks.first(where: awaitsDecision)
    }

    /// Records that the user opened landing's diff.
    public func markReviewed(_ landing: PRLanding) {
        reviewed.insert(landing.sha)
    }

    /// Whether Push is open: orchd allows it and the user has seen the change.
    public func canPush(_ landing: PRLanding) -> Bool {
        landing.canLand && reviewed.contains(landing.sha) && acting == nil
    }

    /// Whether what task proposed is known to be unable to land as it is.
    public func isOutOfDate(_ task: TaskRecord) -> Bool {
        switch landings[task.id]?.state {
        case .stale, .invalid: true
        default: false
        }
    }

    /// Reads what task proposed, checked against the PR's current head. The
    /// newest read wins. A view that goes away or reloads cancels its read,
    /// which is neither a failure nor a reason to skip the next one.
    public func load(_ taskID: String) async {
        let generation = (generations[taskID] ?? 0) + 1
        generations[taskID] = generation
        loading.insert(taskID)
        defer { if generations[taskID] == generation { loading.remove(taskID) } }
        do {
            let fresh = try await api.landing(taskID: taskID)
            guard generations[taskID] == generation else { return }
            landings[taskID] = fresh
            errors[taskID] = nil
        } catch {
            guard generations[taskID] == generation, !Task.isCancelled else { return }
            errors[taskID] = DaemonMonitor.describe(error)
        }
    }

    /// Pushes the commit the user was shown.
    public func land(_ landing: PRLanding) async {
        await act(landing.taskID) { [api] in try await api.land(taskID: landing.taskID, sha: landing.sha) }
    }

    public func undo(_ taskID: String) async {
        await act(taskID) { [api] in try await api.undoLanding(taskID: taskID) }
    }

    public func discard(_ taskID: String) async {
        await act(taskID) { [api] in try await api.discardLanding(taskID: taskID, restore: false) }
    }

    public func restore(_ taskID: String) async {
        await act(taskID) { [api] in try await api.discardLanding(taskID: taskID, restore: true) }
    }

    private func act(_ taskID: String, _ body: @escaping @Sendable () async throws -> PRLanding) async {
        guard acting == nil else { return }
        acting = taskID
        defer { acting = nil }
        do {
            landings[taskID] = try await body()
            errors[taskID] = nil
        } catch {
            errors[taskID] = DaemonMonitor.describe(error)
            // The PR may be why it was refused, and the card should say so.
            if let fresh = try? await api.landing(taskID: taskID) { landings[taskID] = fresh }
        }
        await refreshDecisions()
    }

    /// Reads task's activity until cancelled while running is true, and once
    /// otherwise. The view restarts it when the task settles.
    public func followActivity(_ taskID: String, running: Bool) async {
        while !Task.isCancelled {
            await refreshActivity(taskID)
            guard running else { return }
            do { try await sleep(Self.activityInterval) } catch { return }
        }
    }

    public func refreshActivity(_ taskID: String) async {
        do {
            activity[taskID] = try await api.taskActivity(taskID: taskID)
            activityErrors[taskID] = nil
        } catch {
            guard !Task.isCancelled else { return }
            activityErrors[taskID] = DaemonMonitor.describe(error)
        }
    }
}
