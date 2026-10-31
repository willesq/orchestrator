import Foundation
import Testing
@testable import OrchKit

@Suite struct DispatchDraftTests {
    @Test(arguments: [
        (DispatchDraft(body: "fix it"), nil as String?),
        (DispatchDraft(agent: "", body: "fix it"), nil),
        (DispatchDraft(agent: "my_agent-2", body: "x"), nil),
        (DispatchDraft(agent: "Dev", body: "x"), "Agent names are lowercase letters, digits, dashes and underscores."),
        (DispatchDraft(agent: "-dev", body: "x"), "Agent names are lowercase letters, digits, dashes and underscores."),
        (DispatchDraft(agent: "../x", body: "x"), "Agent names are lowercase letters, digits, dashes and underscores."),
        (DispatchDraft(body: "  \n "), "Describe the task."),
        (DispatchDraft(body: "x", cwd: "relative/dir"), "The working directory must be an absolute path."),
        (DispatchDraft(body: "x", cwd: "/Users/me/repo"), nil),
    ])
    func problem(draft: DispatchDraft, expected: String?) {
        #expect(draft.problem == expected)
    }

    @Test func pastedNewlineInCwdIsTrimmedAndPathNormalized() {
        let draft = DispatchDraft(agent: " dev ", body: "b", cwd: "/Users/me/../me/./repo//\n", model: " m ")
        #expect(draft.problem == nil)
        #expect(draft.normalized == DispatchDraft(agent: "dev", body: "b", cwd: "/Users/me/repo", model: "m"))
        #expect(DispatchDraft(body: "b", cwd: " \n").normalized.cwd.isEmpty)
    }

    @Test func bodyLimitCountsBytesNotCharacters() {
        let atLimit = String(repeating: "a", count: DispatchDraft.maxBodyBytes)
        #expect(DispatchDraft(body: atLimit).problem == nil)
        let emoji = String(repeating: "é", count: DispatchDraft.maxBodyBytes / 2 + 1)
        #expect(DispatchDraft(body: emoji).problem?.hasPrefix("The task is") == true)
    }

    @Test func taskRecordHelpers() {
        #expect(TaskRecord(id: "t", body: "\n  first line  \nsecond").headline == "first line")
        #expect(TaskRecord(id: "t").headline.isEmpty)
        #expect(TaskRecord(id: "t", status: "complete").isFinished && TaskRecord(id: "t", status: "failed").isFinished)
        #expect(!TaskRecord(id: "t", status: "in_progress").isFinished)
        #expect(RouteInfo(provider: "claude-code", model: "opus", effort: "high").label == "claude-code/opus (high)")
        #expect(RouteInfo(model: "opus").label == "opus")
        #expect(TaskStatusFilter.allCases.map(\.rawValue) == ["", "pending", "in_progress", "complete", "failed", "blocked", "interrupted", "cancelled"])
        #expect(Set(TaskRating.allCases.map(\.label)).count == 3)
    }
}

private struct Boom: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }
private struct Plain: Error, CustomStringConvertible { var description = "invalid agent" }

private final class FakeTasks: TasksAPI, @unchecked Sendable {
    var tasks: [TaskRecord] = [
        TaskRecord(id: "a", body: "first", status: "complete"),
        TaskRecord(id: "b", body: "second", status: "in_progress"),
    ]
    var fail = false
    var dispatchDelay: Duration = .zero
    private(set) var listed: [(TaskStatusFilter, Int)] = []
    private(set) var rated: [(String, TaskRating)] = []
    private(set) var dispatched: [DispatchDraft] = []
    private(set) var recommended: [String] = []

    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] {
        listed.append((status, limit))
        if fail { throw Boom() }
        return tasks
    }

    func task(id: String) async throws -> TaskRecord? { tasks.first { $0.id == id } }

    func rate(id: String, _ rating: TaskRating) async throws {
        if fail { throw Boom() }
        rated.append((id, rating))
        if let i = tasks.firstIndex(where: { $0.id == id }) {
            let t = tasks[i]
            tasks[i] = TaskRecord(id: t.id, body: t.body, status: t.status, rating: rating)
        }
    }

    var dispatchError: (any Error)?

    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome {
        dispatched.append(draft)
        try? await Task.sleep(for: dispatchDelay)
        if let dispatchError { throw dispatchError }
        if fail { throw Boom() }
        return DispatchOutcome(id: "task-1", workerEnabled: false, route: RouteInfo(model: "opus"))
    }

    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) {
        recommended.append(taskKind)
        if fail { throw Boom() }
        return (RouteInfo(taskKind: taskKind, model: "sonnet"), true)
    }

    private(set) var retried: [String] = []
    private(set) var cancelled: [String] = []

    func retry(id: String) async throws -> String {
        if fail { throw Boom() }
        retried.append(id)
        return id + "#retry-1"
    }

    func cancel(id: String) async throws -> String {
        if fail { throw Boom() }
        cancelled.append(id)
        return "stopping its running session"
    }

    /// Events each watchTasks call delivers before ending, then the error
    /// it ends with (nil: the stream ends normally).
    var streams: [([TaskEvent], (any Error)?)] = []
    private(set) var watched: [TaskStatusFilter] = []

    func watchTasks(status: TaskStatusFilter, limit: Int, onEvent: @escaping @Sendable (TaskEvent) async -> Void) async throws {
        watched.append(status)
        guard !streams.isEmpty else { throw CancellationError() }
        let (events, end) = streams.removeFirst()
        for event in events { await onEvent(event) }
        if let end { throw end }
    }
}

@MainActor
@Suite struct TasksModelTests {
    @Test func refreshPassesFilterAndSelects() async {
        let api = FakeTasks()
        let model = TasksModel(api: api)
        model.filter = .complete
        await model.refresh()
        #expect(model.loaded && model.tasks.count == 2)
        #expect(api.listed.first?.0 == .complete && api.listed.first?.1 == TasksModel.pageSize)
        model.selectedID = "b"
        #expect(model.selected?.body == "second")
    }

    @Test func refreshFailureKeepsList() async {
        let api = FakeTasks()
        let model = TasksModel(api: api)
        await model.refresh()
        api.fail = true
        await model.refresh()
        #expect(model.tasks.count == 2 && model.lastError == "unavailable")
    }

    @Test func ratingAFinishedTaskReloadsIt() async {
        let api = FakeTasks()
        let model = TasksModel(api: api)
        await model.refresh()
        model.selectedID = "a"
        await model.rateSelected(.rework)
        #expect(api.rated.map(\.0) == ["a"])
        #expect(model.selected?.rating == .rework && model.rating == nil)
    }

    @Test func unfinishedOrUnselectedTasksAreNotRated() async {
        let api = FakeTasks()
        let model = TasksModel(api: api)
        await model.refresh()
        await model.rateSelected(.good)
        model.selectedID = "b"
        await model.rateSelected(.good)
        #expect(api.rated.isEmpty)
    }

    @Test func ratingFailureIsReported() async {
        let api = FakeTasks()
        let model = TasksModel(api: api)
        await model.refresh()
        model.selectedID = "a"
        api.fail = true
        await model.rateSelected(.bad)
        #expect(model.lastError == "Rating failed: unavailable")
    }
}

@MainActor
@Suite struct DispatchModelTests {
    @Test func invalidDraftNeverReachesOrchd() async {
        let api = FakeTasks()
        let model = DispatchModel(api: api)
        model.draft.body = "  "
        await model.submit()
        #expect(model.lastError == "Describe the task." && api.dispatched.isEmpty)
    }

    @Test func submitTrimsSendsAndClearsTheBody() async {
        let api = FakeTasks()
        var refreshed = 0
        let model = DispatchModel(api: api) { refreshed += 1 }
        model.draft = DispatchDraft(agent: " dev ", body: "do the thing", cwd: " /tmp/x\n", taskKind: "code", model: " ")
        await model.submit()
        #expect(api.dispatched == [DispatchDraft(agent: "dev", body: "do the thing", cwd: "/tmp/x", taskKind: "code", model: "")])
        #expect(model.outcome == DispatchOutcome(id: "task-1", workerEnabled: false, route: RouteInfo(model: "opus")))
        #expect(model.draft.body.isEmpty && refreshed == 1 && !model.isSubmitting)
    }

    @Test func failureKeepsTheDraft() async {
        let api = FakeTasks()
        api.dispatchError = Plain()
        let model = DispatchModel(api: api)
        model.draft.body = "keep me"
        await model.submit()
        #expect(model.lastError == "Dispatch failed: invalid agent" && model.draft.body == "keep me" && model.outcome == nil)
    }

    @Test(arguments: ["RPCError: deadline exceeded", "RPCError: unavailable", "cancelled"])
    func lostAnswerWarnsAndRefreshes(error: String) async {
        let api = FakeTasks()
        api.dispatchError = Plain(description: error)
        var refreshed = 0
        let model = DispatchModel(api: api) { refreshed += 1 }
        model.draft.body = "maybe queued"
        await model.submit()
        #expect(model.lastError?.contains("may have been queued") == true)
        #expect(refreshed == 1 && model.draft.body == "maybe queued")
    }

    @Test func aSendFinishingAfterResetDoesNotRepopulate() async {
        let api = FakeTasks()
        api.dispatchDelay = .milliseconds(200)
        var refreshed = 0
        let model = DispatchModel(api: api) { refreshed += 1 }
        model.draft.body = "late"
        let send = Task { await model.submit() }
        try? await Task.sleep(for: .milliseconds(50))
        model.reset()
        await send.value
        #expect(model.outcome == nil && model.lastError == nil && !model.isSubmitting && refreshed == 0)
        #expect(api.dispatched.count == 1)
    }

    @Test func pinningAModelClearsTheRecommendation() async {
        let api = FakeTasks()
        let model = DispatchModel(api: api)
        model.draft.taskKind = "code"
        await model.refreshRecommendation()
        #expect(model.recommendation != nil)
        model.draft.model = "codex/gpt"
        await model.refreshRecommendation()
        #expect(model.recommendation == nil)
    }

    @Test func doubleSubmitQueuesOnce() async {
        let api = FakeTasks()
        api.dispatchDelay = .milliseconds(200)
        let model = DispatchModel(api: api)
        model.draft.body = "once"
        let first = Task { await model.submit() }
        try? await Task.sleep(for: .milliseconds(50))
        await model.submit()
        await first.value
        #expect(api.dispatched.count == 1)
    }

    @Test func recommendationFollowsKindUnlessAModelIsPinned() async {
        let api = FakeTasks()
        let model = DispatchModel(api: api)
        await model.refreshRecommendation()
        #expect(model.recommendation == nil && api.recommended.isEmpty)
        model.draft.taskKind = "review"
        await model.refreshRecommendation()
        #expect(model.recommendation?.route?.model == "sonnet" && model.recommendation?.fromTable == true)
        model.draft.model = "codex/gpt"
        await model.refreshRecommendation()
        #expect(model.recommendation == nil && api.recommended == ["review"])
    }

    @Test func recommendationFailureClearsIt() async {
        let api = FakeTasks()
        let model = DispatchModel(api: api)
        model.draft.taskKind = "code"
        await model.refreshRecommendation()
        api.fail = true
        await model.refreshRecommendation()
        #expect(model.recommendation == nil)
    }

    @Test func resetClearsEverything() async {
        let api = FakeTasks()
        let model = DispatchModel(api: api)
        model.draft = DispatchDraft(body: "x", taskKind: "code")
        await model.refreshRecommendation()
        await model.submit()
        model.reset()
        #expect(model.draft == DispatchDraft() && model.outcome == nil && model.recommendation == nil && model.lastError == nil)
    }
}
