import Foundation
import Observation

/// What the hand-off form holds for one PR. Until the user edits the brief it
/// follows the goal, push permission and notes.
public struct PRHandoffDraft: Sendable, Equatable {
    public var goal: PRHandoffGoal
    /// Off by default: the agent reports a diff unless the user lets it push.
    public var mayPush = false
    public var notes = ""
    /// The brief as the user edited it; nil while it is generated.
    public var editedBrief: String?
    public var agent = "dev"
    public var model = ""

    public init(goal: PRHandoffGoal) {
        self.goal = goal
    }

    /// The draft's first goal for detail: the most blocking one, or Something else.
    public init(detail: PRDetail, review: PRReview?) {
        self.init(goal: PRHandoff.suggestions(detail: detail, review: review).first ?? .other)
    }

    /// Something else says nothing by itself, so it needs notes or an edited
    /// brief: an agent sent off without them can only stop and ask.
    public var needsInstructions: Bool {
        goal == .other && editedBrief == nil && notes.allSatisfy(\.isWhitespace)
    }

    /// ownPR false keeps the agent from pushing or running the PR's code.
    public func brief(detail: PRDetail, review: PRReview?, ownPR: Bool) -> String {
        editedBrief ?? PRHandoff.brief(goal: goal, detail: detail, review: review, mayPush: mayPush, notes: notes,
                                       ownPR: ownPR)
    }
}

/// Hands PRs to background agents and follows the tasks working on them, so
/// the PRs pane shows which PRs an agent is on and can stop it.
@MainActor
@Observable
public final class PRHandoffModel {
    /// How often the task list is read again while the PRs pane is open.
    public static let pollInterval: Duration = .seconds(5)

    public private(set) var tasks: [TaskRecord] = []
    public private(set) var loaded = false
    /// The task a cancel is in flight for.
    public private(set) var cancelling: String?
    public private(set) var lastError: String?
    /// Sends the brief; its outcome and error are the form's.
    public let dispatch: DispatchModel

    private let api: any TasksAPI
    private let sleep: @Sendable (Duration) async throws -> Void

    public init(api: any TasksAPI, sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }) {
        self.api = api
        self.sleep = sleep
        dispatch = DispatchModel(api: api)
    }

    /// Reads the task list until cancelled.
    public func follow() async {
        while !Task.isCancelled {
            await refresh()
            do { try await sleep(Self.pollInterval) } catch { break }
        }
    }

    public func refresh() async {
        do {
            tasks = try await api.listTasks(status: .all, limit: TasksModel.pageSize)
            loaded = true
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// The tasks working on url, unfinished first.
    public func tasks(for url: String) -> [TaskRecord] {
        PRHandoff.tasks(for: url, in: tasks)
    }

    /// The unfinished task working on url, if any.
    public func working(on url: String) -> TaskRecord? {
        tasks(for: url).first(where: \.isUnsettled)
    }

    /// Dispatches draft's brief for detail's PR as a code task. Reports whether
    /// orchd queued it.
    @discardableResult
    public func handOff(_ draft: PRHandoffDraft, detail: PRDetail, review: PRReview?, ownPR: Bool) async -> Bool {
        // reset clears isSubmitting, so a second send must stop here or it
        // would queue a second agent on the same branch.
        guard !dispatch.isSubmitting else { return false }
        // A previous hand-off's outcome must not read as this one's.
        dispatch.reset()
        dispatch.draft = DispatchDraft(agent: draft.agent, body: draft.brief(detail: detail, review: review, ownPR: ownPR),
                                       taskKind: "code", model: draft.model)
        await dispatch.submit()
        guard dispatch.outcome != nil, dispatch.lastError == nil else { return false }
        await refresh()
        return true
    }

    /// Stops a task working on a PR.
    public func cancel(_ id: String) async {
        guard cancelling == nil else { return }
        cancelling = id
        defer { cancelling = nil }
        do {
            _ = try await api.cancel(id: id)
            lastError = nil
            await refresh()
        } catch {
            lastError = "Cancel failed: \(DaemonMonitor.describe(error))"
        }
    }
}
