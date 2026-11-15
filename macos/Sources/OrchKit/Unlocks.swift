import Foundation
import Observation

/// One operator unlock (SP-077): a terminal's request for operator scope
/// while a spaces policy is applied and, once confirmed here, the grant to
/// the requesting process and what it starts.
public struct OperatorUnlock: Identifiable, Equatable, Sendable {
    public enum State: String, Sendable {
        /// Waiting for the user to confirm or deny it here.
        case pending
        /// Confirmed here; the user types its code in the terminal, and
        /// nothing is granted until they do.
        case confirmed
        case active
    }

    public let id: String
    public let state: State
    /// How long the grant lasts once confirmed.
    public let forSeconds: Int64
    /// The requesting process.
    public let pid: Int32
    /// Its command line, as orchd read it from the process table.
    public let command: String
    /// Its parents' programs, nearest first, so the user sees which
    /// terminal asked.
    public let ancestry: [String]
    /// For a confirmed request, the code the user types in the requesting
    /// terminal. orchd gives it to Orch.app only.
    public let code: String
    /// The requesting process's executable, as the kernel reports it.
    public let executable: String
    /// Its controlling terminal (e.g. ttys003), as the kernel reports it;
    /// empty for none. The terminal that asked prints it too.
    public let tty: String
    public let requestedAt: Date
    /// When a pending request lapses, or an active grant ends.
    public let expiresAt: Date

    public init(id: String, state: State, forSeconds: Int64, pid: Int32, command: String, ancestry: [String],
                requestedAt: Date, expiresAt: Date, code: String = "", executable: String = "", tty: String = "") {
        self.id = id
        self.state = state
        self.forSeconds = forSeconds
        self.pid = pid
        self.command = command
        self.ancestry = ancestry
        self.requestedAt = requestedAt
        self.expiresAt = expiresAt
        self.code = code
        self.executable = executable
        self.tty = tty
    }

    /// The terminal it came from, for the panel.
    public var terminalText: String { tty.isEmpty ? "no terminal" : tty }

    public var isPending: Bool { state == .pending }
    public var isActive: Bool { state == .active }

    /// The duration asked for, e.g. "15 min" or "1 h".
    public var durationText: String { Self.duration(TimeInterval(forSeconds)) }

    /// Where it was asked from, e.g. "zsh (31) ← Terminal (30)".
    public var origin: String {
        ancestry.isEmpty ? "no terminal (its parent is launchd)" : ancestry.joined(separator: " ← ")
    }

    /// The time left at `now`, as m:ss or h:mm:ss; "0:00" once ended.
    public func countdown(at now: Date) -> String {
        let left = max(0, Int(expiresAt.timeIntervalSince(now).rounded(.up)))
        let (h, m, s) = (left / 3600, left % 3600 / 60, left % 60)
        return h > 0 ? String(format: "%d:%02d:%02d", h, m, s) : String(format: "%d:%02d", m, s)
    }

    /// What the system asks the user to authenticate for. It names no
    /// text the requester chose.
    public var authenticationReason: String {
        "confirm an operator unlock for \(durationText) (process \(pid))"
    }

    static func duration(_ seconds: TimeInterval) -> String {
        let s = Int(seconds)
        switch s {
        case ..<60: return "\(s) s"
        case 3600: return "1 h"
        default: return s % 60 == 0 ? "\(s / 60) min" : "\(s / 60) min \(s % 60) s"
        }
    }
}

/// The unlock RPCs Orch.app uses; orchd answers them only for Orch.app
/// itself, recognized by its code signature.
public protocol UnlocksAPI: Sendable {
    func unlocks() async throws -> [OperatorUnlock]
    func confirmUnlock(id: String, allow: Bool) async throws
    /// Ends an unlock, or every one when `id` is empty; returns how many.
    func lockUnlock(id: String) async throws -> Int
}

/// Asks the person at the Mac to prove it before an unlock is confirmed
/// (Touch ID or the login password), so a program driving the app's buttons
/// cannot confirm one (RC-711). Returns false when they cancel or fail.
public protocol UnlockConfirmer: Sendable {
    func confirm(reason: String) async -> Bool
}

/// Keeps the pending and active unlocks current for the menu bar panel and
/// confirms, denies or ends them.
@MainActor
@Observable
public final class UnlocksModel {
    public private(set) var unlocks: [OperatorUnlock] = []
    public private(set) var lastError: String?
    /// Unlocks with an action in flight.
    public private(set) var busy: Set<String> = []
    /// What went wrong with an action, per unlock.
    public private(set) var errors: [String: String] = [:]
    /// Called with requests that were not pending at the previous read, so
    /// the user hears of one without opening the panel.
    public var onNewRequests: (@MainActor ([OperatorUnlock]) -> Void)?

    private let api: any UnlocksAPI
    private let confirmer: any UnlockConfirmer
    private let interval: Duration
    private var seen: Set<String> = []
    private var poll: Task<Void, Never>?

    public init(api: any UnlocksAPI, confirmer: any UnlockConfirmer, interval: Duration = .seconds(2)) {
        self.api = api
        self.confirmer = confirmer
        self.interval = interval
    }

    public var pending: [OperatorUnlock] { unlocks.filter(\.isPending) }
    public var confirmed: [OperatorUnlock] { unlocks.filter { $0.state == .confirmed } }
    public var active: [OperatorUnlock] { unlocks.filter(\.isActive) }

    public func load() async {
        do {
            let loaded = try await api.unlocks()
            let fresh = loaded.filter { $0.isPending && !seen.contains($0.id) }
            seen = Set(loaded.map(\.id))
            unlocks = loaded
            lastError = nil
            errors = errors.filter { id, _ in seen.contains(id) }
            if !fresh.isEmpty { onNewRequests?(fresh) }
        } catch {
            // Reason: with orchd away or refusing, nothing can be confirmed,
            // so nothing stale is offered.
            unlocks = []
            lastError = spacesRefusal(error)
        }
    }

    /// Starts reading the unlocks; calling it again is a no-op.
    public func start() {
        guard poll == nil else { return }
        poll = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.load()
                try? await Task.sleep(for: self.interval)
            }
        }
    }

    public func stop() {
        poll?.cancel()
        poll = nil
    }

    /// Confirms a pending unlock once the person at the Mac authenticates.
    public func confirm(_ unlock: OperatorUnlock) async {
        await act(on: unlock) {
            guard await self.confirmer.confirm(reason: unlock.authenticationReason) else {
                return "Not confirmed: authentication was cancelled or failed."
            }
            try await self.api.confirmUnlock(id: unlock.id, allow: true)
            return nil
        }
    }

    /// Denies a pending unlock; refusing needs no authentication.
    public func deny(_ unlock: OperatorUnlock) async {
        await act(on: unlock) {
            try await self.api.confirmUnlock(id: unlock.id, allow: false)
            return nil
        }
    }

    /// Ends an unlock: one in force, or a confirmed one whose code was not
    /// typed yet.
    public func lock(_ unlock: OperatorUnlock) async {
        await act(on: unlock) {
            _ = try await self.api.lockUnlock(id: unlock.id)
            return nil
        }
    }

    private func act(on unlock: OperatorUnlock, _ action: @MainActor () async throws -> String?) async {
        guard !busy.contains(unlock.id) else { return }
        busy.insert(unlock.id)
        errors[unlock.id] = nil
        do {
            errors[unlock.id] = try await action()
        } catch {
            errors[unlock.id] = spacesRefusal(error)
        }
        busy.remove(unlock.id)
        await load()
    }
}
