import OrchKit
import SwiftUI

struct MemoryView: View {
    let search: SearchModel
    let sources: SourcesModel

    @State private var tab = Tab.search

    enum Tab: String, CaseIterable {
        case search = "Search", sources = "Sources"
    }

    var body: some View {
        VStack(spacing: 0) {
            Picker("", selection: $tab) {
                ForEach(Tab.allCases, id: \.self) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .frame(maxWidth: 260)
            .padding(.vertical, 10)
            Divider()
            switch tab {
            case .search: SearchPane(model: search)
            case .sources: SourcesPane(model: sources)
            }
        }
        .navigationTitle("Memory")
    }
}

private struct SearchPane: View {
    @Bindable var model: SearchModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                TextField("Search memory", text: $model.query.text)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { Task { await model.search() } }
                Picker("Mode", selection: $model.query.mode) {
                    ForEach(SearchMode.allCases) { Text($0.label).tag($0) }
                }
                .fixedSize()
                TextField("Type", text: $model.query.sourceType)
                    .textFieldStyle(.roundedBorder)
                    .frame(width: 110)
                Stepper("Top \(model.query.limit)", value: $model.query.limit, in: 1...SearchQuery.maxResults)
                    .fixedSize()
                Button("Search") { Task { await model.search() } }
                    .keyboardShortcut(.defaultAction)
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
            results
        }
    }

    @ViewBuilder private var results: some View {
        if model.isSearching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if let result = model.result {
            if result.hits.isEmpty {
                ContentUnavailableView.search(text: model.answered?.text ?? "")
            } else {
                List(Array(result.hits.enumerated()), id: \.offset) { index, hit in
                    HitRow(rank: index + 1, hit: hit)
                }
            }
        } else {
            ContentUnavailableView("Search your memory", systemImage: "magnifyingglass",
                                   description: Text("Indexed docs, repos and past sessions."))
        }
    }
}

private struct HitRow: View {
    let rank: Int
    let hit: SearchHit

    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Text("\(rank).").foregroundStyle(.secondary)
                Text(String(format: "%.2f", hit.score)).font(.caption.monospacedDigit())
                    .padding(.horizontal, 4).background(.quaternary, in: .rect(cornerRadius: 3))
                Text(verbatim: hit.sourceURI).font(.callout.monospaced()).lineLimit(1).truncationMode(.middle)
                    .textSelection(.enabled)
                Spacer()
                if !hit.sourceType.isEmpty { Text(verbatim: hit.sourceType).font(.caption).foregroundStyle(.secondary) }
                if let when = hit.startedAt ?? hit.ingestedAt {
                    Text(when, format: .dateTime.year().month().day()).font(.caption).foregroundStyle(.secondary)
                }
            }
            // Indexed content is untrusted: plain Text, never Markdown.
            Text(verbatim: hit.text)
                .font(.callout)
                .lineLimit(expanded ? nil : 4)
                .textSelection(.enabled)
            Button(expanded ? "Less" : "More") { expanded.toggle() }
                .buttonStyle(.link)
                .font(.caption)
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
                    ForEach(model.knownTypes, id: \.self) { Text(verbatim: $0).tag($0) }
                }
                .fixedSize()
                .onChange(of: model.typeFilter) { Task { await model.refresh() } }
                Spacer()
                if let list = model.list {
                    Text("\(list.totalSources) sources · \(list.totalChunks) chunks")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Button { Task { await model.refresh() } } label: { Image(systemName: "arrow.clockwise") }
            }
            .padding(12)
            if let error = model.lastError {
                Text("Could not list sources: \(error)").foregroundStyle(.red).padding(.horizontal, 12)
            }
            Divider()
            Table(model.list?.sources ?? []) {
                TableColumn("Source") { Text(verbatim: $0.uri).font(.callout.monospaced()).textSelection(.enabled) }
                TableColumn("Type") { Text(verbatim: $0.sourceType) }.width(110)
                TableColumn("Scope") { Text(verbatim: $0.scope) }.width(140)
                TableColumn("Chunks") { Text("\($0.chunks)").monospacedDigit() }.width(60)
                TableColumn("Stored") { source in
                    if let at = source.ingestedAt { Text(at, format: .relative(presentation: .named)) }
                }
                .width(110)
            }
        }
        .task { if model.list == nil { await model.refresh() } }
    }
}
