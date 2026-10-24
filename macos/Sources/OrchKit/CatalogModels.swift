import Foundation
import Observation

/// Drives the Docs tab: the synced repos' docs, one doc rebuilt from memory,
/// and searches within that doc or its repo.
@MainActor
@Observable
public final class DocsModel {
    public enum Scope: String, CaseIterable, Identifiable, Sendable {
        case doc = "This doc", repo = "This repo"
        public var id: String { rawValue }
    }

    /// One repo's docs in the list.
    public struct RepoGroup: Identifiable, Equatable, Sendable {
        public var id: String { "\(owner)/\(repo)" }
        public let owner: String
        public let repo: String
        public let info: DocRepo?
        public let files: [DocFile]
    }

    public private(set) var catalog: DocCatalog?
    public private(set) var lastError: String?
    public private(set) var isLoading = false
    public private(set) var selected: DocFile?
    public private(set) var document: RebuiltDocument?
    public private(set) var documentError: String?
    public private(set) var isLoadingDocument = false
    /// Filters the list by path, repo or owner.
    public var filter = ""
    public private(set) var scope = Scope.doc
    public let search: SearchModel

    private let api: any CatalogAPI
    /// Bumped per selection, so a slow load cannot replace a newer one.
    private var generation = 0

    public init(api: any CatalogAPI & MemoryAPI) {
        self.api = api
        search = SearchModel(api: api)
    }

    public func load() async {
        guard !isLoading else { return }
        isLoading = true
        defer { isLoading = false }
        do {
            catalog = try await api.listDocs()
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// The docs passing the filter, grouped by repo in owner and repo order.
    public var groups: [RepoGroup] {
        guard let catalog else { return [] }
        let needle = filter.trimmingCharacters(in: .whitespaces)
        let files = needle.isEmpty ? catalog.files : catalog.files.filter {
            "\($0.owner)/\($0.repo)/\($0.path)".range(of: needle, options: .caseInsensitive) != nil
        }
        var infos: [String: DocRepo] = [:]
        for owner in catalog.owners {
            for repo in owner.repos { infos["\(owner.owner)/\(repo.repo)"] = repo }
        }
        let grouped = Dictionary(grouping: files) { "\($0.owner)/\($0.repo)" }
        return grouped.keys.sorted { $0.lowercased() < $1.lowercased() }.map { key in
            let files = grouped[key]!.sorted { $0.path.lowercased() < $1.path.lowercased() }
            return RepoGroup(owner: files[0].owner, repo: files[0].repo, info: infos[key], files: files)
        }
    }

    public var fileCount: Int { Self.fileCount(groups) }

    /// Chunks the shown docs hold in the store.
    public var chunkCount: Int { Self.chunkCount(groups) }

    public static func fileCount(_ groups: [RepoGroup]) -> Int { groups.reduce(0) { $0 + $1.files.count } }

    public static func chunkCount(_ groups: [RepoGroup]) -> Int {
        groups.reduce(0) { $0 + $1.files.reduce(0) { $0 + $1.chunks } }
    }

    /// Shows one doc: rebuilds it from memory and narrows searches to it.
    public func select(_ file: DocFile?) async {
        generation += 1
        let mine = generation
        selected = file
        document = nil
        documentError = nil
        resetSearch()
        guard let file else {
            isLoadingDocument = false
            return
        }
        isLoadingDocument = true
        let outcome: Result<RebuiltDocument, any Error>
        do {
            outcome = .success(try await api.document(uri: file.sourceURI))
        } catch {
            outcome = .failure(error)
        }
        guard mine == generation else { return }
        switch outcome {
        case .success(let doc): document = doc
        case .failure(let error): documentError = DaemonMonitor.describe(error)
        }
        isLoadingDocument = false
    }

    public func setScope(_ scope: Scope) {
        self.scope = scope
        resetSearch()
    }

    /// Clears the results and points the search at the current doc or repo.
    private func resetSearch() {
        let text = search.query.text
        search.query = SearchQuery(text: text)
        switch (scope, selected) {
        case (.doc, let file?): search.query.sourceURI = file.sourceURI
        case (.repo, let file?): search.query.allTags = file.repoTags
        case (_, nil): break
        }
        search.clear()
    }
}

/// Drives the Sources tab: sources registered for refresh on a cadence, and
/// adding, editing, refreshing, removing and searching within them.
@MainActor
@Observable
public final class RegisteredSourcesModel {
    public private(set) var sources: [RegisteredSource] = []
    public private(set) var loaded = false
    public private(set) var lastError: String?
    /// What the last change did, shown under the list.
    public private(set) var message: String?
    public private(set) var busy = false
    public private(set) var selectedID: String?
    public let search: SearchModel

    private let api: any CatalogAPI
    /// Bumped per load, so an older reply cannot replace a newer one.
    private var generation = 0

    public init(api: any CatalogAPI & MemoryAPI) {
        self.api = api
        search = SearchModel(api: api)
    }

    public var selected: RegisteredSource? { sources.first { $0.id == selectedID } }

    public func load() async {
        generation += 1
        let mine = generation
        do {
            let fresh = try await api.registeredSources()
            guard mine == generation else { return }
            sources = fresh
            loaded = true
            lastError = nil
            if let id = selectedID, !sources.contains(where: { $0.id == id }) { select(nil) }
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    public func select(_ id: String?) {
        selectedID = id
        let text = search.query.text
        search.query = SearchQuery(text: text)
        if let source = selected {
            search.query.sourceURI = source.searchSourceURI
            search.query.allTags = source.searchAllTags
        }
        search.clear()
    }

    /// Adds a source, or saves an edit of `editing`. Returns whether orchd
    /// stored it; on failure `lastError` says why.
    @discardableResult
    public func save(_ draft: SourceDraft, editing: RegisteredSource? = nil) async -> Bool {
        if let problem = draft.problem {
            lastError = problem
            return false
        }
        busy = true
        defer { busy = false }
        do {
            let saved = try await api.saveSource(draft, replacing: editing?.id)
            message = editing == nil ? "Added \(saved.uri). It is fetched on the next scan." : "Saved \(saved.uri)."
            lastError = nil
            await load()
            select(saved.id)
            return true
        } catch {
            lastError = DaemonMonitor.describe(error)
            return false
        }
    }

    @discardableResult
    public func remove(_ source: RegisteredSource, purge: Bool) async -> Bool {
        busy = true
        defer { busy = false }
        do {
            let result = try await api.removeSource(id: source.id, purge: purge)
            message = purge
                ? "Removed \(source.uri) and deleted \(result.sources) indexed source\(result.sources == 1 ? "" : "s") (\(result.chunks) chunks)."
                : "Removed \(source.uri). What it indexed is still in memory."
            lastError = nil
            await load()
            return true
        } catch {
            lastError = DaemonMonitor.describe(error)
            return false
        }
    }

    /// Refreshes one source, or all when `source` is nil.
    public func refresh(_ source: RegisteredSource?) async {
        busy = true
        defer { busy = false }
        do {
            try await api.refreshSource(id: source?.id)
            message = source == nil ? "Refreshing every source now." : "Refreshing \(source!.uri) now."
            lastError = nil
            await load()
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }
}
