import Foundation
import Observation

/// Drives the Memory pane's Browse tab: every chunk matching the filters,
/// unranked, a page at a time until orchd's cursor runs out. Unlike a
/// ranked search it reaches the end of any set.
@MainActor
@Observable
public final class BrowseModel {
    /// The menus' filters and page size.
    public var query = BrowseQuery()
    /// `key:value` filters typed in the filter field; words are ignored.
    public var filterText = ""
    public private(set) var parsed = ParsedMemoryQuery()
    public private(set) var hits: [SearchHit] = []
    /// How many chunks match, from the first page.
    public private(set) var total: Int?
    public private(set) var isLoading = false
    public private(set) var lastError: String?
    /// Whether a listing has answered since the filters last changed.
    public private(set) var loaded = false
    /// The query `hits` answer.
    public private(set) var answered: BrowseQuery?

    private let api: any MemoryAPI
    private var nextCursor = ""
    /// Bumped by every new listing, so a page of an older one that answers
    /// late is dropped instead of appended.
    private var generation = 0
    private var current: Task<Void, Never>?

    public init(api: any MemoryAPI) {
        self.api = api
    }

    /// More pages remain after the ones loaded.
    public var hasMore: Bool { loaded && !nextCursor.isEmpty }

    /// Starts the listing over from the first page with the current filters.
    public func load() async {
        current?.cancel()
        generation += 1
        parsed = MemoryFilterParser.parse(filterText)
        if !parsed.text.isEmpty {
            parsed.notes.append("Browse lists by filters only, so \u{201C}\(displaySafe(parsed.text))\u{201D} was ignored; "
                + "use Search to rank by words.")
        }
        let q = parsed.applied(to: query).normalized
        hits = []
        total = nil
        nextCursor = ""
        loaded = false
        answered = q
        await fetch(q, cursor: "", generation: generation)
    }

    /// Appends the next page; ignored while a page loads or at the end.
    public func loadMore() async {
        guard hasMore, !isLoading, let q = answered else { return }
        await fetch(q, cursor: nextCursor, generation: generation)
    }

    /// Loads the next page when `hit` is the last row shown, so scrolling
    /// to the end keeps going.
    public func rowAppeared(at index: Int) async {
        if index >= hits.count - 1 { await loadMore() }
    }

    /// Drops one typed filter and lists again.
    public func remove(_ chip: MemoryFilterChip) async {
        filterText = MemoryFilterParser.removing(chip, from: filterText)
        await load()
    }

    /// Lists one source's chunks, replacing any `uri:` filter.
    public func within(source uri: String) async {
        var text = filterText
        for chip in MemoryFilterParser.parse(text).chips.reversed() where chip.key == .uri {
            text = MemoryFilterParser.removing(chip, from: text)
        }
        if let token = MemoryFilterParser.token(.uri, uri) {
            filterText = text.isEmpty ? token : "\(text) \(token)"
        } else {
            filterText = text
            query.sourceURI = uri
        }
        await load()
    }

    private func fetch(_ q: BrowseQuery, cursor: String, generation mine: Int) async {
        isLoading = true
        let task = Task { [api] in
            let outcome: Result<BrowsePage, any Error>
            do {
                outcome = .success(try await api.browseChunks(q, cursor: cursor))
            } catch {
                outcome = .failure(error)
            }
            guard mine == self.generation else { return }
            switch outcome {
            case .success(let page):
                self.hits += page.hits
                if let total = page.total { self.total = total }
                self.nextCursor = page.nextCursor
                self.loaded = true
                self.lastError = nil
            case .failure(let error):
                self.lastError = DaemonMonitor.describe(error)
            }
            self.isLoading = false
        }
        current = task
        await task.value
    }

    /// "Showing 50 of 1,234", or that every match is shown.
    public var footer: String? {
        guard loaded, !hits.isEmpty else { return nil }
        let of = total.map { " of \($0.formatted())" } ?? ""
        return hasMore ? "Showing \(hits.count.formatted())\(of)" : "All \(hits.count.formatted()) shown"
    }
}

/// The collections the app may read, loaded once for the filter menus.
@MainActor
@Observable
public final class CollectionsModel {
    public private(set) var collections: [CollectionInfo] = []
    /// Spaces are on, so each space has its own collection.
    public private(set) var spacesOn = false
    public private(set) var loaded = false
    public private(set) var lastError: String?

    private let api: any MemoryAPI

    public init(api: any MemoryAPI) {
        self.api = api
    }

    /// Loads the list the first time it is asked for.
    public func loadOnce() async {
        if !loaded { await refresh() }
    }

    public func refresh() async {
        do {
            let list = try await api.listCollections()
            collections = list.collections
            spacesOn = list.spacesOn
            loaded = true
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// The menu label for `name`, or the name when it is not listed.
    public func label(for name: String) -> String {
        collections.first { $0.name == name }?.label(spacesOn: spacesOn) ?? displaySafe(name)
    }
}

/// One whole source rebuilt from its chunks (`GetDocument`), for a hit's
/// Open whole source.
@MainActor
@Observable
public final class SourceDocumentModel {
    public private(set) var document: RebuiltDocument?
    public private(set) var isLoading = false
    public private(set) var lastError: String?
    /// The source being shown or loaded; nil when closed.
    public private(set) var uri: String?

    private let api: any CatalogAPI
    private var generation = 0

    public init(api: any CatalogAPI) {
        self.api = api
    }

    public func open(_ uri: String) async {
        generation += 1
        let mine = generation
        self.uri = uri
        document = nil
        lastError = nil
        isLoading = true
        do {
            let doc = try await api.document(uri: uri)
            guard mine == generation else { return }
            document = doc
        } catch {
            guard mine == generation else { return }
            lastError = DaemonMonitor.describe(error)
        }
        isLoading = false
    }

    public func close() {
        generation += 1
        uri = nil
        document = nil
        lastError = nil
        isLoading = false
    }
}
