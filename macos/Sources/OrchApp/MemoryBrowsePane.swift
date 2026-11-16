import OrchKit
import SwiftUI

/// Memory → Browse: every chunk the filters match, unranked and in storage
/// order, loaded a page at a time to the end.
struct BrowsePane: View {
    @Bindable var model: BrowseModel
    let sources: SourcesModel
    let collections: CollectionsModel?
    let actions: HitActions

    @State private var selection: Int?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            MemoryFilterBar(
                text: $model.filterText, prompt: "Filters, e.g. repo:orchestrator type:repo_doc since:7d",
                submitTitle: "Browse", submit: { Task { await model.load() } }, collections: collections,
                collection: $model.query.collections.single, sourceType: $model.query.sourceType, types: sources.knownTypes,
                pageSize: $model.query.pageSize, pageSizes: BrowseQuery.pageSizeChoices,
                chips: model.parsed.chips, notes: model.parsed.notes,
                remove: { chip in Task { await model.remove(chip) } }, syntaxHelp: MemoryFilterSyntax.browse
            )
            .padding(12)
            if let total = model.total {
                Text("\(total.formatted()) chunk\(total == 1 ? "" : "s") match, unranked, in storage order")
                    .textStyle(.caption).padding(.horizontal, 12).padding(.bottom, 6)
            }
            if let error = model.lastError {
                Text(verbatim: "Browse failed: \(displaySafe(error))").foregroundStyle(.red).padding(.horizontal, 12).padding(.bottom, 6)
            }
            Divider()
            results.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .task {
            if sources.list == nil { await sources.refresh() }
            if model.answered == nil { await model.load() }
        }
        .onChange(of: model.answered) { selection = nil }
    }

    @ViewBuilder private var results: some View {
        if model.hits.isEmpty {
            if model.isLoading {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if model.loaded {
                EmptyState(symbol: "tray", title: "Nothing matches", message: "No stored chunk matches these filters.")
            } else {
                EmptyState(symbol: "list.bullet.rectangle", title: "Browse your memory",
                           message: "Every stored chunk the filters match, unranked, to the end of the list.")
            }
        } else {
            AdaptiveSplit(primaryWidth: .init(min: 340, ideal: 480), secondaryWidth: .init(min: 320, ideal: 400, max: 640),
                          stacking: .detail(shown: selection != nil, backTitle: "Chunks", back: { selection = nil }),
                          secondaryPresent: selection != nil) {
                List(selection: $selection) {
                    ForEach(Array(model.hits.enumerated()), id: \.offset) { index, hit in
                        HitRow(rank: nil, hit: hit)
                            .tag(index)
                            // Reaching the last row loads the next page.
                            .task { await model.rowAppeared(at: index) }
                    }
                    footer
                }
            } secondary: {
                if let selection, model.hits.indices.contains(selection) {
                    HitInspector(hit: model.hits[selection], actions: actions, ranked: false)
                }
            }
        }
    }

    @ViewBuilder private var footer: some View {
        VStack(alignment: .leading, spacing: 6) {
            if model.isLoading {
                ProgressView().controlSize(.small)
            } else if model.hasMore {
                Button("Load more") { Task { await model.loadMore() } }
            }
            if let text = model.footer { Text(verbatim: text).textStyle(.caption) }
        }
        .padding(.vertical, 6)
        .selectionDisabled()
    }
}
