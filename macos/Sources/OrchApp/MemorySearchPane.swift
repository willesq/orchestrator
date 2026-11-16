import OrchKit
import SwiftUI

/// Memory → Search: a ranked search of one batch of up to 100 hits, shown a
/// page at a time, with a hit's every field in the inspector.
struct SearchPane: View {
    @Bindable var model: SearchModel
    let sources: SourcesModel
    let collections: CollectionsModel?
    let actions: HitActions

    @State private var selection: Int?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            MemoryFilterBar(
                text: $model.query.text, prompt: "Search memory, e.g. retry policy repo:orchestrator since:7d",
                submitTitle: "Search", submit: { Task { await model.search() } }, collections: collections,
                collection: $model.query.collections.single, sourceType: $model.query.sourceType, types: sources.knownTypes,
                mode: $model.query.mode, pageSize: $model.query.limit, pageSizes: SearchQuery.limitChoices,
                minRelevance: $model.minRelevance, chips: model.parsed.chips, notes: model.parsed.notes,
                remove: { chip in Task { await model.remove(chip) } }
            )
            .padding(12)
            if let error = model.lastError {
                Text(verbatim: "Search failed: \(displaySafe(error))").foregroundStyle(.red).padding(.horizontal, 12).padding(.bottom, 6)
            }
            if let fallback = model.result?.fallback {
                Text(verbatim: "Answered by meaning only: \(displaySafe(fallback))")
                    .font(.caption).foregroundStyle(.orange).padding(.horizontal, 12).padding(.bottom, 6)
            }
            Divider()
            results.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .task { if sources.list == nil { await sources.refresh() } }
        .onChange(of: model.answered) { selection = nil }
    }

    @ViewBuilder private var results: some View {
        if model.isSearching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if let result = model.result {
            if result.hits.isEmpty {
                EmptyState(symbol: "magnifyingglass", title: "No results",
                           message: "Nothing in memory matches \u{201C}\(displaySafe(model.answered?.text ?? ""))\u{201D}. "
                               + "Try other words, fewer filters, or any type.")
            } else {
                let hits = model.visibleHits
                AdaptiveSplit(primaryWidth: .init(min: 340, ideal: 480), secondaryWidth: .init(min: 320, ideal: 400, max: 640),
                              stacking: .detail(shown: selection != nil, backTitle: "Results", back: { selection = nil }),
                              secondaryPresent: selection != nil) {
                    List(selection: $selection) {
                        ForEach(Array(hits.enumerated()), id: \.offset) { index, hit in
                            HitRow(rank: index + 1, hit: hit).tag(index)
                        }
                        footer
                    }
                } secondary: {
                    if let selection, hits.indices.contains(selection) {
                        HitInspector(hit: hits[selection], actions: actions)
                    }
                }
            }
        } else {
            VStack(spacing: 0) {
                EmptyState(symbol: "magnifyingglass", title: "Search your memory",
                           message: "Indexed docs, repos and past sessions. Type filters such as type:repo_doc or since:7d "
                               + "into the field to narrow it.")
                    .frame(maxHeight: 260)
                if !model.recent.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Recent searches").textStyle(.caption)
                        ForEach(model.recent, id: \.self) { text in
                            Button { Task { await model.repeatSearch(text) } } label: {
                                Label { Text(verbatim: displaySafe(text)) } icon: { Image(systemName: "clock.arrow.circlepath") }
                            }
                            .buttonStyle(.link)
                        }
                    }
                    .frame(maxWidth: 360, alignment: .leading)
                }
                Spacer()
            }
            .frame(maxWidth: .infinity)
        }
    }

    /// "Showing 20 of 100", with Show more while the batch has more.
    @ViewBuilder private var footer: some View {
        if let text = model.footer {
            VStack(alignment: .leading, spacing: 6) {
                if model.canShowMore {
                    Button("Show \(min(model.pageSize, model.matchingHits.count - model.visibleHits.count)) more") {
                        model.showMore()
                    }
                }
                Text(verbatim: text).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
            }
            .padding(.vertical, 6)
            .selectionDisabled()
        }
    }
}
