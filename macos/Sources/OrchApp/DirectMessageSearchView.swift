import OrchKit
import SwiftUI

/// The Memory pane's Direct messages tab (SP-083): the operator's search of
/// chat, direct messages included, through orchd's SearchDirectMessages.
/// It has the Search tab's filters (typed `key:value` filters, Collection,
/// Type, Match, page size and relevance) over one batch of up to 100, and a
/// result's every field in the inspector. Each result names the tenant and
/// space orchd stored it under. While screen-share-safe mode is on it shows
/// no direct message and runs no search; a refused or failed search says why.
struct DirectMessageSearchView: View {
    @Bindable var model: DirectMessageSearchModel
    var collections: CollectionsModel?
    /// The source types for the Type menu.
    var types: [String] = []

    @State private var selection: Int?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            MemoryFilterBar(
                text: $model.query.text, prompt: "Search chat and direct messages, e.g. release since:7d",
                submitTitle: "Search", submit: { Task { await model.search() } }, collections: collections,
                collection: $model.query.collections.single, sourceType: $model.query.sourceType,
                types: typeChoices(known: types, current: DirectMessageSearchModel.chatSourceType),
                mode: $model.query.mode, pageSize: $model.query.limit, pageSizes: SearchQuery.limitChoices,
                minRelevance: $model.minRelevance, chips: model.parsed.chips, notes: model.parsed.notes,
                remove: { chip in Task { await model.remove(chip) } }, syntaxHelp: MemoryFilterSyntax.directMessages
            )
            .padding(12)
            .disabled(model.failure == .sharing)
            Text("Only Orch.app can run this search. Agents, tasks and terminals never see direct messages.")
                .textStyle(.caption)
                .padding(.horizontal, 12)
                .padding(.bottom, 8)
            if let fallback = model.fallback {
                Text(verbatim: "Answered by meaning only: \(fallback)")
                    .font(.caption).foregroundStyle(.orange).padding(.horizontal, 12).padding(.bottom, 8)
            }
            Divider()
            results.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        // Results held when sharing starts are dropped, not just hidden.
        .onChange(of: model.failure == .sharing, initial: true) { _, sharing in
            if sharing { model.clear() }
        }
        .onChange(of: model.answered) { selection = nil }
    }

    @ViewBuilder private var results: some View {
        if let failure = model.failure {
            EmptyState(symbol: failure.kind == .refused ? "hand.raised" : "exclamationmark.triangle",
                       title: failure.title, message: failure.detail)
                .frame(maxWidth: 520)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if model.isSearching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if model.hasSearched && model.hits.isEmpty {
            EmptyState(symbol: "magnifyingglass", title: "No results",
                       message: "No chat message matches. Try other words, fewer filters, or any type.")
        } else if model.hasSearched {
            let hits = model.visibleHits
            AdaptiveSplit(primaryWidth: .init(min: 300, ideal: 460), secondaryWidth: .init(min: 300, ideal: 380, max: 640),
                          stacking: .detail(shown: selection != nil, backTitle: "Results", back: { selection = nil }),
                          secondaryPresent: selection != nil) {
                List(selection: $selection) {
                    ForEach(Array(hits.enumerated()), id: \.offset) { index, hit in DirectMessageRow(hit: hit).tag(index) }
                    footer
                }
            } secondary: {
                if let selection, hits.indices.contains(selection) {
                    // Other reads leave direct messages out, so the only
                    // narrowing offered is this search's own.
                    HitInspector(hit: hits[selection].detail,
                                 actions: HitActions(searchWithin: { uri in Task { await model.within(source: uri) } }),
                                 sensitive: true)
                }
            }
        } else {
            EmptyState(symbol: "bubble.left.and.bubble.right", title: "Search direct messages",
                       message: "Chat channels and direct messages from your connected workspaces, with the tenant and space each came from.")
                .frame(maxWidth: 520)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
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

/// One result: where it came from (tenant, space, source) and its text.
struct DirectMessageRow: View {
    let hit: DirectMessageHit

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            // Reason: a flow, so the chips wrap whole in the companion column.
            FlowLayout(spacing: 6) {
                Chip(text: hit.tenantLabel, symbol: "building.2", help: "Tenant").fixedSize()
                Chip(text: hit.spaceLabel, tone: hit.space.isEmpty ? .neutral : .accent, symbol: "square.3.layers.3d",
                     help: hit.space.isEmpty ? "Stored with spaces off or before a policy, so in no space" : "Space")
                    .fixedSize()
                if let when = hit.startedAt {
                    Text(when, format: .dateTime.year().month().day()).textStyle(.caption).fixedSize()
                }
                Text(String(format: "%.2f", hit.score)).textStyle(.caption).monospacedDigit().help("Relevance score")
            }
            Text(verbatim: hit.sourceURI)
                .textStyle(.codeCaption)
                .lineLimit(1)
                .truncationMode(.middle)
                .textSelection(.enabled)
            // Chat text is written by other people: plain Text, never Markdown.
            Text(verbatim: hit.text)
                .font(.callout)
                .lineLimit(6)
                .textSelection(.enabled)
        }
        .padding(.vertical, 4)
        .accessibilityElement(children: .combine)
    }
}
