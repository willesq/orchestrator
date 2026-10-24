import Foundation

/// The orchd calls behind the Docs and Sources tabs.
public protocol CatalogAPI: Sendable {
    func listDocs() async throws -> DocCatalog
    /// One indexed source rebuilt from its chunks.
    func document(uri: String) async throws -> RebuiltDocument
    func registeredSources() async throws -> [RegisteredSource]
    /// Adds or updates a source; `replacing` is the entry an edit that changed
    /// the kind or address replaces.
    func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource
    /// Unregisters a source; `purge` also deletes what it indexed.
    func removeSource(id: String, purge: Bool) async throws -> PurgeResult
    /// Refreshes one source, or every source when `id` is nil, now.
    func refreshSource(id: String?) async throws
}

public struct DocCatalog: Sendable, Equatable {
    public let owners: [DocOwner]
    public let files: [DocFile]
    /// More files exist than one reply carries.
    public let truncated: Bool

    public init(owners: [DocOwner], files: [DocFile], truncated: Bool = false) {
        self.owners = owners
        self.files = files
        self.truncated = truncated
    }
}

/// A GitHub account whose repos' docs orchd keeps in sync.
public struct DocOwner: Sendable, Equatable, Identifiable {
    public var id: String { owner }
    public let owner: String
    public let refreshSeconds: Int64
    public let lastSynced: Date?
    public let status: String
    public let repos: [DocRepo]

    public init(owner: String, refreshSeconds: Int64 = 0, lastSynced: Date? = nil, status: String = "", repos: [DocRepo] = []) {
        self.owner = owner
        self.refreshSeconds = refreshSeconds
        self.lastSynced = lastSynced
        self.status = status
        self.repos = repos
    }
}

public struct DocRepo: Sendable, Equatable, Identifiable {
    public var id: String { repo }
    public let repo: String
    /// The default-branch commit last indexed.
    public let headSHA: String
    public let lastSynced: Date?
    public let files: Int
    public let status: String
    public let webURL: String

    public init(repo: String, headSHA: String = "", lastSynced: Date? = nil, files: Int = 0, status: String = "", webURL: String = "") {
        self.repo = repo
        self.headSHA = headSHA
        self.lastSynced = lastSynced
        self.files = files
        self.status = status
        self.webURL = webURL
    }
}

/// One synced markdown file, as the store holds it.
public struct DocFile: Sendable, Equatable, Identifiable, Hashable {
    public var id: String { sourceURI }
    public let sourceURI: String
    public let owner: String
    public let repo: String
    public let path: String
    public let updatedAt: Date?
    /// The file on GitHub at the indexed commit.
    public let webURL: String
    /// Chunks the store holds for it.
    public let chunks: Int

    public init(sourceURI: String, owner: String, repo: String, path: String, updatedAt: Date? = nil, webURL: String = "",
                chunks: Int = 0) {
        self.sourceURI = sourceURI
        self.owner = owner
        self.repo = repo
        self.path = path
        self.updatedAt = updatedAt
        self.webURL = webURL
        self.chunks = chunks
    }

    /// The tags every doc of this file's repo carries, for searching the repo.
    public var repoTags: [String] { ["owner:\(owner)", "repo:\(repo)"] }
}

/// A source rebuilt from its stored chunks: what retrieval sees, close to but
/// not exactly the original. Its text is indexed content, shown verbatim.
public struct RebuiltDocument: Sendable, Equatable {
    public let sourceURI: String
    public let sourceType: String
    public let text: String
    public let chunks: Int
    /// The chunk order was incomplete; the text is best effort.
    public let malformed: Bool
    /// Too large to show; the text is empty.
    public let oversize: Bool
    public let webURL: String

    public init(sourceURI: String, sourceType: String = "", text: String, chunks: Int = 0,
                malformed: Bool = false, oversize: Bool = false, webURL: String = "") {
        self.sourceURI = sourceURI
        self.sourceType = sourceType
        self.text = text
        self.chunks = chunks
        self.malformed = malformed
        self.oversize = oversize
        self.webURL = webURL
    }
}

public enum SourceKind: String, Sendable, CaseIterable, Identifiable {
    case web, repo, owner

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .web: "Web page"
        case .repo: "Local repo"
        case .owner: "Local owner folder"
        }
    }

    public var placeholder: String {
        switch self {
        case .web: "https://example.com/docs"
        case .repo: "/Users/you/code/project"
        case .owner: "folder name under a known root"
        }
    }
}

/// A source registered for refresh on a cadence.
public struct RegisteredSource: Sendable, Equatable, Identifiable {
    public let id: String
    public let kind: SourceKind?
    public let uri: String
    public let scope: String
    public let tags: [String]
    public let refreshSeconds: Int64
    public let lastFetched: Date?
    /// new | ok | missing | error
    public let status: String
    public let webURL: String
    /// What a search within this source passes.
    public let searchSourceURI: String
    public let searchAllTags: [String]

    public init(id: String, kind: SourceKind?, uri: String, scope: String = "", tags: [String] = [], refreshSeconds: Int64 = 86_400,
                lastFetched: Date? = nil, status: String = "new", webURL: String = "", searchSourceURI: String = "",
                searchAllTags: [String] = []) {
        self.id = id
        self.kind = kind
        self.uri = uri
        self.scope = scope
        self.tags = tags
        self.refreshSeconds = refreshSeconds
        self.lastFetched = lastFetched
        self.status = status
        self.webURL = webURL
        self.searchSourceURI = searchSourceURI
        self.searchAllTags = searchAllTags
    }

    /// Whether a search can be narrowed to this source.
    public var isSearchable: Bool { !searchSourceURI.isEmpty || !searchAllTags.isEmpty }

    public var draft: SourceDraft {
        SourceDraft(kind: kind ?? .web, uri: uri, scope: scope, tags: tags.joined(separator: ", "), refreshSeconds: refreshSeconds)
    }
}

public struct PurgeResult: Sendable, Equatable {
    public let sources: Int
    public let chunks: Int

    public init(sources: Int, chunks: Int) {
        self.sources = sources
        self.chunks = chunks
    }
}

/// A source being added or edited. `problem` mirrors orchd's checks so the
/// form can say what is wrong before sending; orchd still validates.
public struct SourceDraft: Sendable, Equatable {
    public static let refreshChoices: [(label: String, seconds: Int64)] = [
        ("Every hour", 3_600), ("Every 6 hours", 21_600), ("Every day", 86_400), ("Every week", 604_800),
    ]

    public var kind: SourceKind
    public var uri: String
    public var scope: String
    /// Comma-separated, as typed.
    public var tags: String
    public var refreshSeconds: Int64

    public init(kind: SourceKind = .web, uri: String = "", scope: String = "", tags: String = "", refreshSeconds: Int64 = 86_400) {
        self.kind = kind
        self.uri = uri
        self.scope = scope
        self.tags = tags
        self.refreshSeconds = refreshSeconds
    }

    public var trimmedURI: String { uri.trimmingCharacters(in: .whitespacesAndNewlines) }

    public var tagList: [String] {
        var out: [String] = []
        for t in tags.split(separator: ",").map({ $0.trimmingCharacters(in: .whitespaces) }) where !t.isEmpty && !out.contains(t) {
            out.append(t)
        }
        return out
    }

    /// Why orchd would refuse this draft, or nil.
    public var problem: String? {
        let uri = trimmedURI
        switch kind {
        case .web:
            guard let url = URL(string: uri), let scheme = url.scheme?.lowercased(), scheme == "http" || scheme == "https",
                  let host = url.host, !host.isEmpty
            else { return "Enter an http or https address." }
            if url.user != nil || url.password != nil { return "Leave credentials out of the address." }
        case .repo:
            if !uri.hasPrefix("/") { return "Enter the repo's full path, starting with /." }
        case .owner:
            if uri.isEmpty || uri.contains("/") || uri.allSatisfy({ $0 == "." }) {
                return "Enter one folder name, without slashes."
            }
            if uri == "local" { return "\u{201C}local\u{201D} is reserved." }
        }
        if uri.count > 2048 { return "The address is too long." }
        if scope.count > 64 { return "Scope is at most 64 characters." }
        let tags = tagList
        if tags.count > 32 { return "Use at most 32 tags." }
        if tags.contains(where: { $0.count > 64 }) { return "Each tag is at most 64 characters." }
        if tags.contains(where: { $0.hasPrefix("owner:") || $0.hasPrefix("repo:") }) {
            return "Tags starting with owner: or repo: are added automatically."
        }
        return nil
    }
}

/// A GitHub file link orchd built for a synced doc, opened only when it is
/// exactly `https://github.com/<owner>/<repo>/blob/<ref>/<path>`.
public func gitHubBlobURL(_ raw: String) -> URL? {
    guard let url = URL(string: raw), url.scheme == "https", url.host == "github.com", url.port == nil,
          url.user == nil, url.password == nil, url.query == nil, url.fragment == nil
    else { return nil }
    let parts = url.path.split(separator: "/", omittingEmptySubsequences: false)
    // "", owner, repo, "blob", ref, path…
    guard parts.count >= 6, parts[0].isEmpty, parts[3] == "blob",
          parts.dropFirst().allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." })
    else { return nil }
    return url
}

/// A web source's address, opened only when it is plain http(s).
public func webSourceURL(_ raw: String) -> URL? {
    guard let url = URL(string: raw), let scheme = url.scheme?.lowercased(), scheme == "http" || scheme == "https",
          let host = url.host, !host.isEmpty, url.user == nil, url.password == nil
    else { return nil }
    return url
}

/// A name from indexed or third-party content (a path, a URI) made safe to
/// show: control, format and bidi characters become visible escapes, so a
/// name cannot pose as another or hide a line break.
public func displaySafe(_ s: String) -> String {
    var out = ""
    for scalar in s.unicodeScalars {
        if scalar.properties.generalCategory == .control || scalar.properties.generalCategory == .format {
            out += String(format: "\\u{%04X}", scalar.value)
        } else {
            out.unicodeScalars.append(scalar)
        }
    }
    return out
}

/// Whether a string is free of control and format characters, so it can be
/// copied for pasting into a terminal as is.
public func isPlain(_ s: String) -> Bool { displaySafe(s) == s }
