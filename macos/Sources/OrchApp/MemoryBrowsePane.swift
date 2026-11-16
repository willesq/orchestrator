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
                remove: { chip in Task { await model.remove(chip) } }
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

/// Memory → Indexed: every source held in memory, by type and collection.
struct SourcesPane: View {
    @Bindable var model: SourcesModel
    let collections: CollectionsModel?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                if let collections {
                    CollectionPicker(model: collections, selection: $model.collections.single)
                        .fixedSize()
                        .onChange(of: model.collections) { Task { await model.refresh() } }
                }
                Picker("Type", selection: $model.typeFilter) {
                    Text("All types").tag("")
                    ForEach(model.knownTypes, id: \.self) { Text(verbatim: displaySafe($0)).tag($0) }
                }
                .fixedSize()
                .onChange(of: model.typeFilter) { Task { await model.refresh() } }
                Spacer()
                if let list = model.list {
                    Text("\(list.totalSources) sources · \(list.totalChunks) chunks").textStyle(.caption)
                }
                Button { Task { await model.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                    .help("Reload the list")
                    .accessibilityLabel("Reload")
            }
            .controlSize(.small)
            .padding(12)
            if let error = model.lastError {
                Text(verbatim: "Could not list sources: \(error)").foregroundStyle(.red).padding(.horizontal, 12)
            }
            Divider()
            Table(model.list?.sources ?? []) {
                TableColumn("Source") {
                    Text(verbatim: displaySafe($0.uri)).font(.callout.monospaced()).lineLimit(1).truncationMode(.middle)
                        .help(displaySafe($0.uri)).textSelection(.enabled)
                }
                .width(min: 240, ideal: 520)
                TableColumn("Type") { Text(verbatim: displaySafe($0.sourceType)) }.width(min: 90, ideal: 140, max: 180)
                TableColumn("Scope") { Text(verbatim: displaySafe($0.scope)) }.width(min: 50, ideal: 80, max: 160)
                TableColumn("Chunks") { Text("\($0.chunks)").monospacedDigit().frame(maxWidth: .infinity, alignment: .trailing) }
                    .width(64)
                TableColumn("Stored") { source in
                    if let at = source.ingestedAt { Text(at, format: .relative(presentation: .named)) }
                }
                .width(110)
            }
        }
        .task { if model.list == nil { await model.refresh() } }
    }
}
