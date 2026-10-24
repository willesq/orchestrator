import Foundation

/// What a scope's PRs and issues include.
public enum GitHubItemsMode: String, Sendable, CaseIterable, Identifiable {
    case involving
    case all

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .involving: "Involving me"
        case .all: "Everything open"
        }
    }
}

/// Which of a scope's repos count.
public enum GitHubRepoMode: String, Sendable, CaseIterable, Identifiable {
    case all
    case include
    case exclude

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .all: "All repos"
        case .include: "Only selected repos"
        case .exclude: "All but selected repos"
        }
    }
}

/// One account orch may watch: the user's own, or an org they belong to.
public struct GitHubScope: Sendable, Equatable, Identifiable {
    public var id: String { owner.lowercased() }
    public let owner: String
    public let isUser: Bool
    public var enabled: Bool
    public var items: GitHubItemsMode
    public var repoMode: GitHubRepoMode
    public var repos: [String]

    public init(owner: String, isUser: Bool, enabled: Bool = false, items: GitHubItemsMode = .involving,
                repoMode: GitHubRepoMode = .all, repos: [String] = []) {
        self.owner = owner
        self.isUser = isUser
        self.enabled = enabled
        self.items = items
        self.repoMode = repoMode
        self.repos = repos
    }

    /// A one-line description of what an enabled scope covers.
    public var coverage: String {
        let repoText = switch repoMode {
        case .all: "all repos"
        case .include: repos.count == 1 ? "1 repo" : "\(repos.count) repos"
        case .exclude: "all but \(repos.count) repo\(repos.count == 1 ? "" : "s")"
        }
        return "\(items.label) · \(repoText)"
    }
}

public struct GitHubScopeList: Sendable, Equatable {
    public let scopes: [GitHubScope]
    /// Set when orchd could not reach GitHub; the stored scopes are still listed.
    public let warning: String?

    public init(scopes: [GitHubScope], warning: String? = nil) {
        self.scopes = scopes
        self.warning = warning
    }
}

public struct GitHubTeam: Sendable, Equatable, Identifiable {
    public var id: String { "\(org)/\(slug)" }
    public let org: String
    public let slug: String
    public let name: String

    public init(org: String, slug: String, name: String) {
        self.org = org
        self.slug = slug
        self.name = name
    }
}

public struct GitHubIdentity: Sendable, Equatable {
    public let login: String
    public let orgs: [String]
    public let teams: [GitHubTeam]

    public init(login: String, orgs: [String] = [], teams: [GitHubTeam] = []) {
        self.login = login
        self.orgs = orgs
        self.teams = teams
    }
}

public struct GitHubRepo: Sendable, Equatable, Identifiable {
    public var id: String { name.lowercased() }
    public let name: String
    public let isPrivate: Bool
    public let isArchived: Bool

    public init(name: String, isPrivate: Bool = false, isArchived: Bool = false) {
        self.name = name
        self.isPrivate = isPrivate
        self.isArchived = isArchived
    }
}

/// Why an item is watched, as orchd's watcher records it.
public enum GitHubReason: String, Sendable, CaseIterable {
    case authored
    case assigned
    case reviewRequested = "review_requested"
    case involved
    case allOpen = "all_open"

    public var label: String {
        switch self {
        case .authored: "Yours"
        case .assigned: "Assigned"
        case .reviewRequested: "Review requested"
        case .involved: "Mentioned"
        case .allOpen: "Open in scope"
        }
    }
}

/// One open PR or issue in an enabled scope. Titles and authors are other
/// people's text and are shown verbatim, never as Markdown.
public struct GitHubItem: Sendable, Equatable, Identifiable {
    public var id: String { url }
    public let url: String
    public let title: String
    public let repo: String
    public let isPR: Bool
    public let number: Int
    public let author: String
    public let updatedAt: Date?
    public let isDraft: Bool
    public let ciState: String
    public let reasons: [GitHubReason]
    public let attention: Attention?

    public init(url: String, title: String, repo: String, isPR: Bool = true, number: Int = 0, author: String = "",
                updatedAt: Date? = nil, isDraft: Bool = false, ciState: String = "", reasons: [GitHubReason] = [],
                attention: Attention? = nil) {
        self.url = url
        self.title = title
        self.repo = repo
        self.isPR = isPR
        self.number = number
        self.author = author
        self.updatedAt = updatedAt
        self.isDraft = isDraft
        self.ciState = ciState
        self.reasons = reasons
        self.attention = attention
    }

    /// The account or org the item belongs to.
    public var owner: String { String(repo.split(separator: "/").first ?? "") }

    /// The item's page, only when it is exactly a github.com pull or issue URL.
    public var webURL: URL? { githubItemURL(url) }
}

public struct GitHubItemList: Sendable, Equatable {
    public let items: [GitHubItem]
    public let updatedAt: Date?

    public init(items: [GitHubItem], updatedAt: Date?) {
        self.items = items
        self.updatedAt = updatedAt
    }
}

/// `raw` as a URL only when it is exactly `https://github.com/<owner>/<repo>/pull|issues/<n>`:
/// the app hands it to the system to open, so nothing else gets through.
public func githubItemURL(_ raw: String) -> URL? {
    guard let components = URLComponents(string: raw),
          components.scheme == "https",
          components.host == "github.com",
          components.port == nil,
          components.user == nil, components.password == nil,
          components.query == nil, components.fragment == nil,
          // The encoded path, so %2F cannot smuggle in a separator, and each name
          // starts alphanumeric, so "." and ".." cannot be segments.
          components.percentEncodedPath.wholeMatch(
              of: /\/[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*\/(pull|issues)\/[0-9]+/
          ) != nil
    else { return nil }
    return components.url
}

/// The user's GitHub monitoring, through orchd.
public protocol GitHubAPI: Sendable {
    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity
    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList
    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope
    func gitHubRepos(owner: String) async throws -> [GitHubRepo]
    func gitHubItems() async throws -> GitHubItemList
}
