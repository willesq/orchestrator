import Foundation
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

    private let api: any TasksAPI

    public init(api: any TasksAPI) {
        self.api = api
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
