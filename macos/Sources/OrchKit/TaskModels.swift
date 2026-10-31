import Foundation
import GRPCCore
import Observation

/// Drives the task list and the selected task's detail.
@MainActor
@Observable
public final class TasksModel {
    public static let pageSize = 100

    public var filter = TaskStatusFilter.all
    public private(set) var tasks: [TaskRecord] = []
    public private(set) var loaded = false
    public private(set) var lastError: String?
    public var selectedID: String?
    public private(set) var rating: String?
    /// The task a retry or cancel is in flight for.
    public private(set) var acting: String?
    /// The last retry or cancel's outcome, for the detail pane.
    public private(set) var actionMessage: String?
    /// True while the list follows orchd's task stream, false while it falls
    /// back to polling (or is reconnecting).
    public private(set) var live = false

    private let api: any TasksAPI
    /// Waits between reconnect attempts and polls; replaced in tests.
    private let sleep: @Sendable (Duration) async throws -> Void

    public init(api: any TasksAPI, sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }) {
        self.api = api
        self.sleep = sleep
    }

    /// How long the list waits between polls when the stream is unavailable.
    public static let pollInterval: Duration = .seconds(10)

    /// Follows orchd's task stream for the current filter until cancelled:
    /// the list updates as tasks change rather than on a poll. A stream that
    /// ends (orchd restarted) or fails is reopened with backoff; an orchd too
    /// old to stream is polled instead. The view restarts this when the
    /// filter changes.
    public func follow() async {
        var delay: Duration = .seconds(1)
        var streams = true
        while !Task.isCancelled {
            if streams {
                do {
                    try await api.watchTasks(status: filter, limit: Self.pageSize) { [weak self] event in
                        await self?.apply(event)
                    }
                    delay = .seconds(1)
                } catch {
                    if Task.isCancelled { break }
                    live = false
                    if Self.isUnimplemented(error) {
                        streams = false
                        continue
                    }
                    lastError = DaemonMonitor.describe(error)
                }
                live = false
                do { try await sleep(delay) } catch { break }
                delay = min(delay * 2, .seconds(30))
            } else {
                await refresh()
                do { try await sleep(Self.pollInterval) } catch { break }
            }
        }
        live = false
    }

    /// Applies one stream event to the list.
    public func apply(_ event: TaskEvent) {
        switch event.kind {
        case .snapshot:
            tasks = event.tasks
            loaded = true
            live = true
            lastError = nil
        case .changed, .tick:
            for task in event.tasks {
                let keep = filter == .all || task.status == filter.rawValue
                if let index = tasks.firstIndex(where: { $0.id == task.id }) {
                    if !keep {
                        tasks.remove(at: index)
                    } else if event.kind == .tick {
                        tasks[index] = tasks[index].merging(tick: task)
                    } else {
                        tasks[index] = task
                    }
                } else if keep, event.kind == .changed {
                    tasks.insert(task, at: 0)
                }
            }
        }
    }

    /// Runs the selected task again; its session resumes where it left off.
    public func retrySelected() async {
        guard let task = selected, task.canRetry, acting == nil else { return }
        await act(on: task.id) { [api] in
            let run = try await api.retry(id: task.id)
            return "Retrying as run \(run); the session resumes where it left off."
        }
    }

    /// Stops the selected task.
    public func cancelSelected() async {
        guard let task = selected, task.canCancel, acting == nil else { return }
        await act(on: task.id) { [api] in "Cancelled: " + (try await api.cancel(id: task.id)) + "." }
    }

    private func act(on id: String, _ body: @escaping @Sendable () async throws -> String) async {
        acting = id
        defer { acting = nil }
        do {
            actionMessage = try await body()
            // The stream delivers the change; this covers a polling list.
            if !live, let fresh = try await api.task(id: id), let index = tasks.firstIndex(where: { $0.id == id }) {
                tasks[index] = fresh
            }
        } catch {
            actionMessage = nil
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Whether orchd lacks the stream (an older version), detected from the
    /// RPC's unimplemented status.
    static func isUnimplemented(_ error: any Error) -> Bool {
        (error as? RPCError)?.code == .unimplemented
    }

    public var selected: TaskRecord? { tasks.first { $0.id == selectedID } }

    public func refresh() async {
        do {
            tasks = try await api.listTasks(status: filter, limit: Self.pageSize)
            loaded = true
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Rates the selected task, then reloads it so the list shows the verdict.
    public func rateSelected(_ verdict: TaskRating) async {
        guard let task = selected, task.isFinished, rating == nil else { return }
        rating = task.id
        defer { rating = nil }
        do {
            try await api.rate(id: task.id, verdict)
            if let fresh = try await api.task(id: task.id), let index = tasks.firstIndex(where: { $0.id == fresh.id }) {
                tasks[index] = fresh
            }
            lastError = nil
        } catch {
            lastError = "Rating failed: \(DaemonMonitor.describe(error))"
        }
    }
}

/// Drives the dispatch form.
@MainActor
@Observable
public final class DispatchModel {
    public var draft = DispatchDraft()
    public private(set) var recommendation: (route: RouteInfo?, fromTable: Bool)?
    public private(set) var outcome: DispatchOutcome?
    public private(set) var isSubmitting = false
    public private(set) var lastError: String?

    private let api: any TasksAPI
    private let afterDispatch: @MainActor () async -> Void
    /// Bumped by `reset`, so a send still in flight when the form closes cannot
    /// repopulate it.
    private var generation = 0

    public init(api: any TasksAPI, afterDispatch: @escaping @MainActor () async -> Void = {}) {
        self.api = api
        self.afterDispatch = afterDispatch
    }

    /// Looks up the route for the draft's kind. Cleared when there is no kind
    /// or a pinned model overrides routing.
    public func refreshRecommendation() async {
        let kind = draft.taskKind
        guard !kind.isEmpty, draft.model.trimmingCharacters(in: .whitespaces).isEmpty else {
            recommendation = nil
            return
        }
        do {
            let found = try await api.recommend(taskKind: kind)
            if draft.taskKind == kind, draft.model.trimmingCharacters(in: .whitespaces).isEmpty {
                recommendation = found
            }
        } catch {
            recommendation = nil
        }
    }

    /// Sends the draft. On success the form clears its body so a second click
    /// cannot queue the same task twice. When the answer is lost (deadline,
    /// connection drop) the task may already be queued, so the list is
    /// refreshed and the message says to check it before retrying.
    public func submit() async {
        guard !isSubmitting else { return }
        if let problem = draft.problem {
            lastError = problem
            return
        }
        isSubmitting = true
        let mine = generation
        defer { if mine == generation { isSubmitting = false } }
        do {
            let result = try await api.dispatch(draft.normalized)
            guard mine == generation else { return }
            outcome = result
            lastError = nil
            draft.body = ""
            await afterDispatch()
        } catch {
            guard mine == generation else { return }
            if Self.mayHaveQueued(error) {
                lastError = "No answer from orchd; the task may have been queued. Check Tasks before sending it again."
                await afterDispatch()
            } else {
                lastError = "Dispatch failed: \(DaemonMonitor.describe(error))"
            }
        }
    }

    public func reset() {
        generation += 1
        draft = DispatchDraft()
        outcome = nil
        lastError = nil
        recommendation = nil
        isSubmitting = false
    }

    /// Errors after which orchd may have stored the task anyway.
    static func mayHaveQueued(_ error: any Error) -> Bool {
        let text = String(describing: error).lowercased()
        return text.contains("deadline") || text.contains("unavailable") || text.contains("cancelled")
    }
}
