import OrchKit
import SwiftUI

struct MemoryView: View {
    let search: SearchModel
    let sources: SourcesModel

    /// The direct-message search (SP-083), from the window's environment.
    @Environment(DirectMessageSearchModel.self) private var directMessages: DirectMessageSearchModel?
    /// Browse, the collections and a hit's whole source, from the window's
    /// environment (MemoryFeature.swift).
    @Environment(BrowseModel.self) private var browse: BrowseModel?
    @Environment(CollectionsModel.self) private var collections: CollectionsModel?
    @Environment(SourceDocumentModel.self) private var document: SourceDocumentModel?

    @State private var tab = Tab.search

    enum Tab: String, CaseIterable {
        // "Indexed": every source held in memory, not the registered
        // sources the Sources tab manages.
        case search = "Search", browse = "Browse", sources = "Indexed", directMessages = "Direct messages"
    }

    var body: some View {
        Group {
            switch tab {
            case .search:
                SearchPane(model: search, sources: sources, collections: collections, actions: actions)
            case .browse:
                if let browse { BrowsePane(model: browse, sources: sources, collections: collections, actions: actions) }
            case .sources: SourcesPane(model: sources, collections: collections, actions: actions)
            case .directMessages:
                if let directMessages {
                    DirectMessageSearchView(model: directMessages, collections: collections, types: sources.knownTypes)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .navigationTitle("Memory")
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("View", selection: $tab) {
                    ForEach(tabs, id: \.self) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
        }
        .task { await collections?.loadOnce() }
        .sheet(isPresented: Binding(get: { document?.uri != nil }, set: { if !$0 { document?.close() } })) {
            if let document { SourceDocumentSheet(model: document) }
        }
    }

    /// What a hit's inspector can do beyond its own buttons.
    private var actions: HitActions {
        HitActions(
            openSource: document.map { document in { uri in Task { await document.open(uri) } } },
            searchWithin: { uri in
                tab = .search
                Task { await search.within(source: uri) }
            },
            browseWithin: browse.map { browse in
                { uri in
                    tab = .browse
                    Task { await browse.within(source: uri) }
                }
            }
        )
    }
}

extension MemoryView {
    /// The tabs offered: Browse and Direct messages only where their models
    /// are provided.
    var tabs: [Tab] {
        Tab.allCases.filter { ($0 != .directMessages || directMessages != nil) && ($0 != .browse || browse != nil) }
    }
}

/// The source types for the Type menus: the store's, plus a chosen one not
/// listed yet.
func typeChoices(known: [String], current: String) -> [String] {
    current.isEmpty || known.contains(current) ? known : known + [current]
}
