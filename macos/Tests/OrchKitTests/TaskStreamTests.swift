import Foundation
import GRPCCore
import Testing
@testable import OrchKit

/// A TasksAPI whose stream plays scripted events; everything else is unused.
private final class StreamTasks: TasksAPI, @unchecked Sendable {
    var streams: [([TaskEvent], (any Error)?)] = []
    var list: [TaskRecord] = []
    var failActions = false
    private(set) var watched: [TaskStatusFilter] = []
    private(set) var listed = 0
    private(set) var retried: [String] = []
    private(set) var cancelled: [String] = []

    func watchTasks(status: TaskStatusFilter, limit: Int, onEvent: @escaping @Sendable (TaskEvent) async -> Void) async throws {
        watched.append(status)
        guard !streams.isEmpty else { throw CancellationError() }
        let (events, end) = streams.removeFirst()
        for event in events { await onEvent(event) }
        if let end { throw end }
    }

    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] {
        listed += 1
        return list
    }

    func task(id: String) async throws -> TaskRecord? { list.first { $0.id == id } }
    func rate(id: String, _ rating: TaskRating) async throws {}
    func retry(id: String) async throws -> String {
        if failActions { throw RPCError(code: .failedPrecondition, message: "still active") }
        retried.append(id)
        return id + "#retry-1"
    }
    func cancel(id: String) async throws -> String {
        if failActions { throw RPCError(code: .failedPrecondition, message: "already ended") }
        cancelled.append(id)
        return "stopping its running session"
    }
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome { DispatchOutcome(id: "x", workerEnabled: true, route: nil) }
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) { (nil, false) }
}

/// Counts sleeps and ends the follow loop after `limit` of them.
private final class Sleeper: @unchecked Sendable {
    let limit: Int
    private(set) var waits: [Duration] = []
    init(limit: Int) { self.limit = limit }
    func sleep(_ d: Duration) async throws {
        waits.append(d)
        if waits.count >= limit { throw CancellationError() }
    }
}

@MainActor
@Suite struct TaskStreamTests {
    let running = TaskRunInfo(state: "running", pid: 42, startedAt: Date(timeIntervalSince1970: 1), sessionID: "sid",
                              cwd: "/repo", resumeCommand: "cd '/repo' && claude --resume sid")

    @Test func applyReplacesUpdatesInsertsAndFilters() {
        let model = TasksModel(api: StreamTasks())
        model.apply(TaskEvent(kind: .snapshot, tasks: [TaskRecord(id: "a", status: "pending"), TaskRecord(id: "b", status: "complete")]))
        #expect(model.loaded && model.live && model.tasks.map(\.id) == ["a", "b"])
        model.apply(TaskEvent(kind: .changed, tasks: [TaskRecord(id: "a", status: "in_progress", run: running)]))
        #expect(model.tasks.first?.status == "in_progress" && model.tasks.first?.run?.pid == 42)
        model.apply(TaskEvent(kind: .changed, tasks: [TaskRecord(id: "c", status: "pending")]))
        #expect(model.tasks.map(\.id) == ["c", "a", "b"])
        // A tick refreshes known tasks but never adds one.
        model.apply(TaskEvent(kind: .tick, tasks: [TaskRecord(id: "z", status: "pending")]))
        #expect(!model.tasks.contains { $0.id == "z" })
        // A tick carries only liveness: it updates status and run and keeps the text.
        model.apply(TaskEvent(kind: .changed, tasks: [TaskRecord(id: "a", body: "the brief", status: "in_progress")]))
        let stalled = TaskRunInfo(state: "stalled")
        model.apply(TaskEvent(kind: .tick, tasks: [TaskRecord(id: "a", body: "", status: "in_progress", run: stalled)]))
        let a = model.tasks.first { $0.id == "a" }
        #expect(a?.body == "the brief" && a?.run == stalled)
        // Under a filter, a task that leaves it is dropped.
        model.filter = .pending
        model.apply(TaskEvent(kind: .changed, tasks: [TaskRecord(id: "c", status: "complete")]))
        #expect(!model.tasks.contains { $0.id == "c" })
        model.apply(TaskEvent(kind: .changed, tasks: [TaskRecord(id: "d", status: "failed")]))
        #expect(!model.tasks.contains { $0.id == "d" })
    }

    @Test func followReconnectsAfterTheStreamEnds() async {
        let api = StreamTasks()
        api.streams = [
            ([TaskEvent(kind: .snapshot, tasks: [TaskRecord(id: "a")])], nil),
            ([TaskEvent(kind: .snapshot, tasks: [TaskRecord(id: "b")])], RPCError(code: .unavailable, message: "orchd restarting")),
        ]
        let sleeper = Sleeper(limit: 2)
        let model = TasksModel(api: api, sleep: sleeper.sleep)
        await model.follow()
        #expect(api.watched.count == 2)
        #expect(model.tasks.map(\.id) == ["b"])
        #expect(sleeper.waits == [.seconds(1), .seconds(2)])
        #expect(model.lastError != nil && !model.live)
    }

    @Test func followPollsWhenOrchdCannotStream() async {
        let api = StreamTasks()
        api.streams = [([], RPCError(code: .unimplemented, message: "unknown method"))]
        api.list = [TaskRecord(id: "p")]
        let sleeper = Sleeper(limit: 2)
        let model = TasksModel(api: api, sleep: sleeper.sleep)
        await model.follow()
        #expect(api.watched.count == 1)
        #expect(api.listed == 2)
        #expect(sleeper.waits == [TasksModel.pollInterval, TasksModel.pollInterval])
        #expect(model.tasks.map(\.id) == ["p"] && !model.live)
    }

    @Test func retryAndCancelTheSelectedTask() async {
        let api = StreamTasks()
        let model = TasksModel(api: api)
        model.apply(TaskEvent(kind: .snapshot, tasks: [
            TaskRecord(id: "done", status: "failed"),
            TaskRecord(id: "run", status: "in_progress", run: running),
        ]))
        model.selectedID = "done"
        await model.retrySelected()
        #expect(api.retried == ["done"] && model.actionMessage?.contains("done#retry-1") == true && model.acting == nil)
        await model.cancelSelected() // a failed task cannot be cancelled
        #expect(api.cancelled.isEmpty)
        model.selectedID = "run"
        await model.retrySelected() // a running task cannot be retried
        #expect(api.retried == ["done"])
        await model.cancelSelected()
        #expect(api.cancelled == ["run"] && model.actionMessage?.contains("stopping") == true)
        api.failActions = true
        await model.cancelSelected()
        #expect(model.actionMessage == nil && model.lastError != nil)
    }

    @Test func runInfoAndPermissions() {
        #expect(running.label == "Running (pid 42)" && !running.isStalled)
        #expect(TaskRunInfo(state: "running").label == "Running")
        #expect(TaskRunInfo(state: "waiting").label == "Waiting for a worker")
        let stalled = TaskRunInfo(state: "stalled")
        #expect(stalled.isStalled && stalled.label.hasPrefix("Stalled"))
        #expect(TaskRecord(id: "x", status: "in_progress", run: stalled).canRetry)
        #expect(!TaskRecord(id: "x", status: "in_progress", run: running).canRetry)
        #expect(TaskRecord(id: "x", status: "complete").canRetry && !TaskRecord(id: "x", status: "complete").canCancel)
        #expect(TaskRecord(id: "x", status: "pending").canCancel)
        #expect(TasksModel.isUnimplemented(RPCError(code: .unimplemented, message: "")))
        #expect(!TasksModel.isUnimplemented(RPCError(code: .unavailable, message: "")))
        #expect(TaskStatusFilter.cancelled.label == "Cancelled" && TaskStatusFilter.interrupted.label == "Interrupted"
            && TaskStatusFilter.blocked.label == "Blocked")
    }

    @Test func mapsTheWireRun() {
        var r = Orchd_V1_TaskRun()
        r.state = "running"
        r.pid = 7
        r.startedAt = 100
        r.sessionID = "sid"
        r.resumeCommand = "claude --resume sid"
        let info = DaemonClient.run(r)
        #expect(info.pid == 7 && info.startedAt == Date(timeIntervalSince1970: 100) && info.lastActivity == nil)
        var t = Orchd_V1_Task()
        t.id = "x"
        t.run = r
        #expect(DaemonClient.task(t).run == info)
        #expect(DaemonClient.task(Orchd_V1_Task()).run == nil)
    }
}
