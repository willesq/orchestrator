import Foundation
import Observation

/// Which of an owner's repos the docs sync covers.
public enum DocsRepoMode: String, Sendable, Equatable, CaseIterable, Identifiable {
    case all, include, exclude

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .all: "Every repo"
        case .include: "Only these repos"
        case .exclude: "Every repo except these"
        }
    }
}

/// A repo the sync has a record of.
public struct DocsOwnerRepoState: Sendable, Equatable {
    public var repo: String
    public var files: Int
    public var status: String

    public init(repo: String, files: Int = 0, status: String = "") {
        self.repo = repo
        self.files = files
        self.status = status
    }

    /// Opted out, with docs still indexed.
    public var keptIndexed: Bool { status == "excluded" && files > 0 }
}

/// An owner registered with the docs sync.
public struct DocsOwner: Sendable, Equatable, Identifiable {
    public var id: String { owner }
    public var owner: String
    public var enabled: Bool
    public var repoMode: DocsRepoMode
    public var selectedRepos: [String]
    public var refreshSecs: Int64
    public var lastSynced: Date?
    public var status: String
    public var error: String
    public var repos: [DocsOwnerRepoState]

    public init(owner: String, enabled: Bool = true, repoMode: DocsRepoMode = .all, selectedRepos: [String] = [],
                refreshSecs: Int64 = 21600, lastSynced: Date? = nil, status: String = "", error: String = "",
                repos: [DocsOwnerRepoState] = []) {
        self.owner = owner
        self.enabled = enabled
        self.repoMode = repoMode
        self.selectedRepos = selectedRepos
        self.refreshSecs = refreshSecs
        self.lastSynced = lastSynced
        self.status = status
        self.error = error
        self.repos = repos
    }

    /// What the owner covers, in a few words.
    public var coverage: String {
        guard enabled else { return "Off" }
        let n = selectedRepos.count
        switch repoMode {
        case .all: return "Every repo"
        case .include: return n == 1 ? "1 repo" : "\(n) repos"
        case .exclude: return n == 1 ? "All but 1 repo" : "All but \(n) repos"
        }
    }

    /// Opted-out repos whose docs are still in memory.
    public var keptRepos: [DocsOwnerRepoState] { repos.filter(\.keptIndexed) }
}

/// A repo offered when choosing what an owner covers.
public struct DocsOwnerRepoChoice: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var isArchived: Bool
    public var isFork: Bool

    public init(name: String, isArchived: Bool = false, isFork: Bool = false) {
        self.name = name
        self.isArchived = isArchived
        self.isFork = isFork
    }
}

/// How many indexed docs a removal deleted.
public struct DocsPurgeResult: Sendable, Equatable {
    public var sources: Int
    public var chunks: Int

    public init(sources: Int = 0, chunks: Int = 0) {
        self.sources = sources
        self.chunks = chunks
    }
}

public protocol DocsOwnersAPI: Sendable {
    func docsOwners() async throws -> [DocsOwner]
    func docsOwnerRepos(owner: String) async throws -> [DocsOwnerRepoChoice]
    func saveDocsOwner(owner: String, enabled: Bool, mode: DocsRepoMode, repos: [String]) async throws -> DocsOwner
    func removeDocsOwner(owner: String, purge: Bool) async throws -> DocsPurgeResult
    func purgeDocs(owner: String, repos: [String]) async throws -> DocsPurgeResult
}

/// One row of the docs opt-in list: an account or org orch knows of, or an
/// owner added by name, registered or not.
public struct DocsOwnerRow: Sendable, Equatable, Identifiable {
    public var id: String { login }
    public let login: String
    /// nil until opted in.
    public let owner: DocsOwner?

    public var isOn: Bool { owner?.enabled ?? false }
}

/// Drives opting GitHub accounts, orgs and repos in and out of the docs sync.
/// A failure stays with its owner, so a view shows it inline.
@MainActor
@Observable
public final class DocsOwnersModel {
    public private(set) var owners: [DocsOwner] = []
    /// Accounts and orgs orch knows of, offered before they are opted in.
    public private(set) var known: [String] = []
    public private(set) var lastError: String?
    public private(set) var message: String?
    public private(set) var pending: Set<String> = []
    public private(set) var ownerErrors: [String: String] = [:]
    private var repoLists: [String: [DocsOwnerRepoChoice]] = [:]
    private var repoErrors: [String: String] = [:]

    private let api: any DocsOwnersAPI
    private let knownOwners: @Sendable () async throws -> [String]

    public init(api: any DocsOwnersAPI, knownOwners: @escaping @Sendable () async throws -> [String]) {
        self.api = api
        self.knownOwners = knownOwners
    }

    public var rows: [DocsOwnerRow] {
        let registered = Dictionary(owners.map { ($0.owner.lowercased(), $0) }, uniquingKeysWith: { a, _ in a })
        var seen = Set<String>()
        var out: [DocsOwnerRow] = []
        for login in known + owners.map(\.owner) where seen.insert(login.lowercased()).inserted {
            out.append(DocsOwnerRow(login: registered[login.lowercased()]?.owner ?? login, owner: registered[login.lowercased()]))
        }
        return out
    }

    public func load() async {
        // Without GitHub the registered owners still show, and can be changed.
        known = (try? await knownOwners()) ?? known
        do {
            owners = try await api.docsOwners()
            lastError = nil
        } catch {
            lastError = displaySafe(DocsModel.refusal(error))
        }
    }

    public static func validLogin(_ login: String) -> Bool {
        login.range(of: #"^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$"#, options: .regularExpression) != nil
    }

    /// Opts an owner in or out, keeping which repos it covers.
    public func setEnabled(_ login: String, _ on: Bool) async {
        let current = owners.first { $0.owner == login }
        await save(login, enabled: on, mode: current?.repoMode ?? .all, repos: current?.selectedRepos ?? [])
    }

    /// Adds an owner by name, opted in.
    public func add(_ login: String) async -> Bool {
        let name = login.trimmingCharacters(in: .whitespacesAndNewlines)
        guard Self.validLogin(name) else {
            message = "\(displaySafe(name)) is not a GitHub account or org name."
            return false
        }
        return await save(name, enabled: true, mode: .all, repos: [])
    }

    /// Sets which repos an owner covers; false when it did not save.
    @discardableResult
    public func setRepos(_ login: String, mode: DocsRepoMode, repos: [String]) async -> Bool {
        let current = owners.first { $0.owner == login }
        return await save(login, enabled: current?.enabled ?? true, mode: mode, repos: mode == .all ? [] : repos.sorted())
    }

    @discardableResult
    private func save(_ login: String, enabled: Bool, mode: DocsRepoMode, repos: [String]) async -> Bool {
        await run(login) { [api] in
            _ = try await api.saveDocsOwner(owner: login, enabled: enabled, mode: mode, repos: repos)
            return nil
        }
    }

    /// Stops tracking an owner, and with purge deletes everything it indexed.
    public func remove(_ login: String, purge: Bool) async {
        await run(login) { [api] in
            let result = try await api.removeDocsOwner(owner: login, purge: purge)
            return purge ? "Removed \(login) and deleted \(result.sources) docs." : "Removed \(login). Its docs stay in memory."
        }
    }

    /// Deletes the indexed docs of the opted-out repos the user was shown.
    public func purgeKept(_ login: String, repos: [String]) async {
        // Never empty: orchd reads no repos as every repo the owner does not cover.
        guard !repos.isEmpty else { return }
        await run(login) { [api] in
            let result = try await api.purgeDocs(owner: login, repos: repos)
            return "Deleted \(result.sources) docs of \(login)."
        }
    }

    @discardableResult
    private func run(_ login: String, _ action: @Sendable () async throws -> String?) async -> Bool {
        pending.insert(login)
        defer { pending.remove(login) }
        ownerErrors[login] = nil
        message = nil
        do {
            message = try await action()
            await load()
            return true
        } catch {
            ownerErrors[login] = displaySafe(DocsModel.refusal(error))
            return false
        }
    }

    public func loadRepos(_ owner: String) async {
        repoErrors[owner] = nil
        do {
            repoLists[owner] = try await api.docsOwnerRepos(owner: owner)
        } catch {
            repoErrors[owner] = displaySafe(DocsModel.refusal(error))
        }
    }

    public func repos(_ owner: String) -> [DocsOwnerRepoChoice]? { repoLists[owner] }
    public func repoError(_ owner: String) -> String? { repoErrors[owner] }
}
