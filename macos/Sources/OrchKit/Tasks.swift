import Foundation

/// The model a task was routed to.
public struct RouteInfo: Sendable, Equatable {
    public let taskKind: String
    public let provider: String
    public let model: String
    public let effort: String
    public let rationale: String
    public let evidenceCount: Int
    public let confidence: String

    public init(taskKind: String = "", provider: String = "", model: String = "", effort: String = "",
                rationale: String = "", evidenceCount: Int = 0, confidence: String = "") {
        self.taskKind = taskKind
        self.provider = provider
        self.model = model
        self.effort = effort
        self.rationale = rationale
        self.evidenceCount = evidenceCount
        self.confidence = confidence
    }

    /// `provider/model`, plus the effort when set.
    public var label: String {
        let name = provider.isEmpty ? model : "\(provider)/\(model)"
        return effort.isEmpty ? name : "\(name) (\(effort))"
    }
}

/// A user's verdict on a finished task.
public enum TaskRating: String, Sendable, CaseIterable, Identifiable {
    case good, rework, bad

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .good: "Good"
        case .rework: "Needed rework"
        case .bad: "Bad"
        }
    }
}

/// The task ledger's status filter; `all` sends no filter.
public enum TaskStatusFilter: String, Sendable, CaseIterable, Identifiable {
    case all = ""
    case pending
    case inProgress = "in_progress"
    case complete
    case failed

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .all: "All"
        case .pending: "Pending"
        case .inProgress: "Running"
        case .complete: "Complete"
        case .failed: "Failed"
        }
    }
}

/// One queued task. Its body and result are agent and user text, shown as
/// plain text only.
public struct TaskRecord: Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let body: String
    public let result: String
    public let status: String
    public let updatedAt: String
    public let costUSD: Double
    public let durationMS: Int64
    public let turns: Int
    public let sessionID: String
    public let models: [String]
    public let prs: [String]
    public let verification: String
    public let route: RouteInfo?
    public let rating: TaskRating?
    public let prOutcome: String

    public init(
        id: String, agent: String = "dev", body: String = "", result: String = "", status: String = "pending",
        updatedAt: String = "", costUSD: Double = 0, durationMS: Int64 = 0, turns: Int = 0, sessionID: String = "",
        models: [String] = [], prs: [String] = [], verification: String = "", route: RouteInfo? = nil,
        rating: TaskRating? = nil, prOutcome: String = ""
    ) {
        self.id = id
        self.agent = agent
        self.body = body
        self.result = result
        self.status = status
        self.updatedAt = updatedAt
        self.costUSD = costUSD
        self.durationMS = durationMS
        self.turns = turns
        self.sessionID = sessionID
        self.models = models
        self.prs = prs
        self.verification = verification
        self.route = route
        self.rating = rating
        self.prOutcome = prOutcome
    }

    /// Only finished tasks can be rated.
    public var isFinished: Bool { status == "complete" || status == "failed" }

    /// The body's first non-empty line, for lists.
    public var headline: String {
        body.split(whereSeparator: \.isNewline).first.map { String($0).trimmingCharacters(in: .whitespaces) } ?? ""
    }
}

/// What the dispatch form sends. orchd validates it too; checking here turns
/// a mistake into a message in the form instead of a failed RPC.
public struct DispatchDraft: Sendable, Equatable {
    /// orchd's limit on a task body, in bytes.
    public static let maxBodyBytes = 64 << 10
    /// The task kinds orchd's routing understands (internal/routing Kinds).
    public static let taskKinds = ["code", "review", "docs", "research", "ops", "classify", "test", "adversarial"]

    public var agent = "dev"
    public var body = ""
    public var cwd = ""
    public var taskKind = ""
    public var model = ""

    public init(agent: String = "dev", body: String = "", cwd: String = "", taskKind: String = "", model: String = "") {
        self.agent = agent
        self.body = body
        self.cwd = cwd
        self.taskKind = taskKind
        self.model = model
    }

    /// Why the draft cannot be sent, or nil when it can.
    public var problem: String? {
        let agent = agent.trimmingCharacters(in: .whitespaces)
        // Same rule as agentrun.ValidAgent.
        if !agent.isEmpty, agent.wholeMatch(of: /[a-z0-9][a-z0-9_-]{0,63}/) == nil {
            return "Agent names are lowercase letters, digits, dashes and underscores."
        }
        if body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return "Describe the task." }
        if body.utf8.count > Self.maxBodyBytes {
            return "The task is \(body.utf8.count) bytes; the limit is \(Self.maxBodyBytes)."
        }
        let cwd = cwd.trimmingCharacters(in: .whitespacesAndNewlines)
        if !cwd.isEmpty, !cwd.hasPrefix("/") { return "The working directory must be an absolute path." }
        return nil
    }

    /// The draft as sent: trimmed fields and a normalized working directory
    /// (no `..`, `.` or duplicate slashes).
    public var normalized: DispatchDraft {
        let cwd = cwd.trimmingCharacters(in: .whitespacesAndNewlines)
        return DispatchDraft(
            agent: agent.trimmingCharacters(in: .whitespaces),
            body: body,
            cwd: cwd.isEmpty ? "" : URL(fileURLWithPath: cwd).standardizedFileURL.path,
            taskKind: taskKind,
            model: model.trimmingCharacters(in: .whitespaces)
        )
    }
}

/// What orchd said about a dispatch.
public struct DispatchOutcome: Sendable, Equatable {
    public let id: String
    /// false: the task waits until a worker is enabled (`queue.worker.enabled`).
    public let workerEnabled: Bool
    public let route: RouteInfo?

    public init(id: String, workerEnabled: Bool, route: RouteInfo?) {
        self.id = id
        self.workerEnabled = workerEnabled
        self.route = route
    }
}

/// The task queue.
public protocol TasksAPI: Sendable {
    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord]
    /// nil when no task has that id.
    func task(id: String) async throws -> TaskRecord?
    func rate(id: String, _ rating: TaskRating) async throws
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome
    /// The model routing would pick for a kind, and whether it came from the
    /// learned routing table rather than a live proposal.
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool)
}
