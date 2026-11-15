import OrchKit
import SwiftUI

struct MemoryView: View {
    let search: SearchModel
    let sources: SourcesModel

    /// The direct-message search (SP-083), from the window's environment.
    @Environment(DirectMessageSearchModel.self) private var directMessages: DirectMessageSearchModel?

    @State private var tab = Tab.search

    enum Tab: String, CaseIterable {
        // "Indexed": every source held in memory, not the registered
        // sources the Sources tab manages.
        case search = "Search", sources = "Indexed", directMessages = "Direct messages"
    }

    var body: some View {
        Group {
            switch tab {
            case .search: SearchPane(model: search, sources: sources)
            case .sources: SourcesPane(model: sources)
            case .directMessages:
                if let directMessages { DirectMessageSearchView(model: directMessages) }
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
    }
}

extension MemoryView {
    /// The tabs offered: Direct messages only where its model is provided.
    var tabs: [Tab] { directMessages == nil ? Tab.allCases.filter { $0 != .directMessages } : Tab.allCases }
}

private struct SearchPane: View {
    @Bindable var model: SearchModel
    let sources: SourcesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    TextField("Search memory", text: $model.query.text)
                        .textFieldStyle(.roundedBorder)
                        .onSubmit { Task { await model.search() } }
                    Button("Search") { Task { await model.search() } }
                        .keyboardShortcut(.defaultAction)
                }
                filters
            }
            .padding(12)
            if let error = model.lastError {
                Text("Search failed: \(error)").foregroundStyle(.red).padding(.horizontal, 12)
            }
            if let fallback = model.result?.fallback {
                Text("Answered by meaning only: \(fallback)")
                    .font(.caption).foregroundStyle(.orange).padding(.horizontal, 12)
            }
            Divider()
            results.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .task { if sources.list == nil { await sources.refresh() } }
    }

    /// Plainly labelled menus in place of free text and a stepper.
    private var filters: some View {
        FlowLayout(spacing: 14) {
            Picker("Match", selection: $model.query.mode) {
                ForEach(SearchMode.allCases) { Text($0.label).tag($0).help($0.help) }
            }
            .fixedSize()
            .help(model.query.mode.help)
            Picker("Type", selection: $model.query.sourceType) {
                Text("Any type").tag("")
                Divider()
                ForEach(typeChoices, id: \.self) { Text(verbatim: displaySafe($0)).tag($0) }
            }
            .fixedSize()
            .help("Only results from sources of this type")
            Picker("Show", selection: $model.query.limit) {
                ForEach(limitChoices, id: \.self) { Text("\($0) results").tag($0) }
            }
            .fixedSize()
        }
        .controlSize(.small)
    }

    /// The store's types, plus a typed-in one that is not listed yet.
    private var typeChoices: [String] {
        let known = sources.knownTypes
        let current = model.query.sourceType
        return current.isEmpty || known.contains(current) ? known : known + [current]
    }

    private var limitChoices: [Int] {
        let current = model.query.limit
        return SearchQuery.limitChoices.contains(current) ? SearchQuery.limitChoices : (SearchQuery.limitChoices + [current]).sorted()
    }

    @ViewBuilder private var results: some View {
        if model.isSearching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if let result = model.result {
            if result.hits.isEmpty {
                EmptyState(symbol: "magnifyingglass", title: "No results",
                           message: "Nothing in memory matches \u{201C}\(model.answered?.text ?? "")\u{201D}. Try other words, or any type.")
            } else {
                List(Array(result.hits.enumerated()), id: \.offset) { index, hit in
                    HitRow(rank: index + 1, hit: hit)
                }
            }
        } else {
            VStack(spacing: 0) {
                EmptyState(symbol: "magnifyingglass", title: "Search your memory",
                           message: "Indexed docs, repos and past sessions.")
                    .frame(maxHeight: 260)
                if !model.recent.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Recent searches").textStyle(.caption)
                        ForEach(model.recent, id: \.self) { text in
                            Button { Task { await model.repeatSearch(text) } } label: {
                                Label { Text(verbatim: text) } icon: { Image(systemName: "clock.arrow.circlepath") }
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
}

private struct HitRow: View {
    let rank: Int
    let hit: SearchHit

    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text("\(rank)").textStyle(.caption).monospacedDigit().frame(minWidth: 16, alignment: .trailing)
                Text(verbatim: hit.sourceTitle).font(.body.weight(.medium)).lineLimit(1)
                if !hit.sourceType.isEmpty { Chip(text: displaySafe(hit.sourceType)) }
                Spacer()
                if let when = hit.startedAt ?? hit.ingestedAt {
                    Text(when, format: .dateTime.year().month().day()).textStyle(.caption)
                }
                Text(String(format: "%.2f", hit.score))
                    .textStyle(.caption)
                    .monospacedDigit()
                    .help("Relevance score")
            }
            Text(verbatim: displaySafe(hit.sourceURI))
                .textStyle(.codeCaption)
                .lineLimit(1)
                .truncationMode(.middle)
                .textSelection(.enabled)
                .padding(.leading, 24)
            // Indexed content is untrusted: plain Text, never Markdown.
            Text(verbatim: expanded ? hit.text : hit.preview)
                .font(.callout)
                .lineLimit(expanded ? nil : 4)
                .textSelection(.enabled)
                .padding(.leading, 24)
            if hit.isLong {
                Button(expanded ? "Show less" : "Show more") { expanded.toggle() }
                    .buttonStyle(.link)
                    .font(.caption)
                    .padding(.leading, 24)
            }
        }
        .padding(.vertical, 4)
    }
}

private struct SourcesPane: View {
    @Bindable var model: SourcesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
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
            .padding(12)
            if let error = model.lastError {
                Text("Could not list sources: \(error)").foregroundStyle(.red).padding(.horizontal, 12)
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
