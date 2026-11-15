import OrchKit
import SwiftUI

/// The Memory pane's Direct messages tab (SP-083): the operator's search of
/// chat, direct messages included, through orchd's SearchDirectMessages.
/// Each result names the tenant and space orchd stored it under. While
/// screen-share-safe mode is on it shows no direct message and runs no
/// search; a refused or failed search says why.
struct DirectMessageSearchView: View {
    @Bindable var model: DirectMessageSearchModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                TextField("Search chat and direct messages", text: $model.query.text)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { Task { await model.search() } }
                Picker("Show", selection: $model.query.limit) {
                    ForEach(SearchQuery.limitChoices, id: \.self) { Text("\($0) results").tag($0) }
                }
                .labelsHidden()
                .fixedSize()
                .controlSize(.small)
                Button("Search") { Task { await model.search() } }
                    .keyboardShortcut(.defaultAction)
            }
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
                       message: "No chat message matches. Try other words.")
        } else if model.hasSearched {
            List(model.hits) { hit in DirectMessageRow(hit: hit) }
        } else {
            EmptyState(symbol: "bubble.left.and.bubble.right", title: "Search direct messages",
                       message: "Chat channels and direct messages from your connected workspaces, with the tenant and space each came from.")
                .frame(maxWidth: 520)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
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
