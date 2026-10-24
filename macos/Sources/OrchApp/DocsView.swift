import OrchKit
import SwiftUI

struct DocsView: View {
    @Bindable var model: DocsModel

    var body: some View {
        HSplitView {
            DocList(model: model)
                .frame(minWidth: 260, idealWidth: 320, maxWidth: 460)
            DocDetail(model: model)
                .frame(minWidth: 380, maxWidth: .infinity, maxHeight: .infinity)
        }
        .navigationTitle("Docs")
        .toolbar {
            ToolbarItem {
                Button { Task { await model.load() } } label: {
                    Label("Reload", systemImage: "arrow.clockwise")
                }
                .disabled(model.isLoading)
                .help("Reload the list of synced docs")
            }
        }
        .task { if model.catalog == nil { await model.load() } }
    }
}

private struct DocList: View {
    @Bindable var model: DocsModel

    var body: some View {
        VStack(spacing: 0) {
            TextField("Filter by path or repo", text: $model.filter)
                .textFieldStyle(.roundedBorder)
                .padding(8)
            if let error = model.lastError {
                Label("orchd: \(error)", systemImage: "bolt.slash")
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal, 8)
            }
            List(selection: Binding(get: { model.selected }, set: { file in Task { await model.select(file) } })) {
                ForEach(model.groups) { group in
                    Section {
                        ForEach(group.files) { file in
                            HStack {
                                Text(verbatim: file.path)
                                    .lineLimit(1)
                                    .truncationMode(.middle)
                                Spacer()
                                Text("\(file.chunks)")
                                    .font(.caption.monospacedDigit())
                                    .foregroundStyle(.tertiary)
                            }
                            .help("\(file.path): \(file.chunks) chunk\(file.chunks == 1 ? "" : "s") in memory")
                            .tag(file)
                        }
                    } header: {
                        RepoHeader(group: group)
                    }
                }
            }
            .overlay {
                if model.catalog != nil && model.groups.isEmpty {
                    Text(model.filter.isEmpty ? "No synced docs are in memory. Add an owner with: orch docs add <owner>" : "No docs match.")
                        .multilineTextAlignment(.center)
                        .foregroundStyle(.secondary)
                        .padding()
                }
            }
            Divider()
            HStack {
                Text("\(model.fileCount) docs · \(model.chunkCount) chunks in memory")
                if model.catalog?.truncated == true {
                    Text("(first \(model.catalog?.files.count ?? 0) shown)")
                }
                Spacer()
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            .padding(6)
        }
    }
}

private struct RepoHeader: View {
    let group: DocsModel.RepoGroup

    var body: some View {
        HStack {
            Text(verbatim: "\(group.owner)/\(group.repo)").font(.subheadline.weight(.semibold))
            Spacer()
            if let info = group.info, let synced = info.lastSynced {
                Text("synced \(synced, format: .relative(presentation: .named))")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

private struct DocDetail: View {
    @Bindable var model: DocsModel

    var body: some View {
        if let file = model.selected {
            VStack(alignment: .leading, spacing: 0) {
                header(file)
                    .padding(12)
                Divider()
                VSplitView {
                    ScopedSearch(search: model.search, scope: Binding(get: { model.scope }, set: { model.setScope($0) }),
                                 onOpen: { uri in openDoc(uri) })
                        .frame(minHeight: 120, idealHeight: 200)
                    DocumentText(model: model)
                        .frame(minHeight: 200, maxHeight: .infinity)
                }
            }
        } else {
            Text("Choose a doc to read it as memory holds it, search it, or open it on GitHub.")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .padding()
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func header(_ file: DocFile) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(verbatim: file.path).font(.title3.weight(.semibold)).textSelection(.enabled)
            HStack(spacing: 6) {
                Text(verbatim: "\(file.owner)/\(file.repo)")
                if let updated = file.updatedAt {
                    Text("· indexed \(updated, format: .relative(presentation: .named))")
                }
            }
            .font(.callout)
            .foregroundStyle(.secondary)
            HStack {
                if let url = gitHubBlobURL(file.webURL) {
                    Link(destination: url) { Label("Open on GitHub", systemImage: "arrow.up.right.square") }
                        .help(url.absoluteString)
                }
                Button {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(file.sourceURI, forType: .string)
                } label: {
                    Label("Copy memory address", systemImage: "doc.on.doc")
                }
                .help(file.sourceURI)
            }
            .controlSize(.small)
        }
    }

    /// A repo-wide hit opens its doc, when the doc is in the list.
    private func openDoc(_ uri: String) {
        guard let file = model.catalog?.files.first(where: { $0.sourceURI == uri }), file != model.selected else { return }
        Task { await model.select(file) }
    }
}

private struct DocumentText: View {
    let model: DocsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("Rebuilt from memory").font(.headline)
                if let doc = model.document {
                    Text("\(doc.chunks) chunk\(doc.chunks == 1 ? "" : "s")").font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .help("Pieced back together from the chunks agents retrieve: close to the original, not exact.")
            if model.isLoadingDocument {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let error = model.documentError {
                Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red).padding(12)
                Spacer()
            } else if let doc = model.document {
                if doc.malformed {
                    Label("Some chunks were out of order; this is a best effort.", systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .padding(.horizontal, 12)
                }
                if doc.oversize {
                    Text("This doc is too large to show here. Open it on GitHub instead.")
                        .foregroundStyle(.secondary)
                        .padding(12)
                    Spacer()
                } else {
                    ScrollView {
                        // Indexed content: shown verbatim, never rendered.
                        Text(verbatim: doc.text)
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(12)
                    }
                    .background(Color(nsColor: .textBackgroundColor))
                }
            }
        }
    }
}

/// A search narrowed to one doc or one repo, with its results.
struct ScopedSearch<Scope: Hashable & Identifiable & RawRepresentable>: View where Scope.RawValue == String, Scope: CaseIterable,
    Scope.AllCases: RandomAccessCollection {
    @Bindable var search: SearchModel
    var scope: Binding<Scope>?
    var onOpen: (String) -> Void = { _ in }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                TextField("Search", text: $search.query.text)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { Task { await search.search() } }
                if let scope {
                    Picker("Within", selection: scope) {
                        ForEach(Scope.allCases) { Text($0.rawValue).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    .fixedSize()
                }
                Button("Search") { Task { await search.search() } }
                    .disabled(search.isSearching)
            }
            if let error = search.lastError {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
            if search.isSearching {
                ProgressView().controlSize(.small)
            } else if let result = search.result {
                if result.hits.isEmpty {
                    Text("No matches.").font(.caption).foregroundStyle(.secondary)
                }
                List(Array(result.hits.enumerated()), id: \.offset) { _, hit in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack {
                            Text(String(format: "%.3f", hit.score)).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                            Button { onOpen(hit.sourceURI) } label: {
                                Text(verbatim: hit.sourceURI).font(.caption).lineLimit(1).truncationMode(.middle)
                            }
                            .buttonStyle(.link)
                        }
                        Text(verbatim: hit.text).font(.callout).lineLimit(4).textSelection(.enabled)
                    }
                }
                .listStyle(.plain)
            }
        }
        .padding(12)
    }
}

/// No scope picker: the source decides the scope.
enum NoScope: String, CaseIterable, Identifiable {
    case only = ""
    var id: String { rawValue }
}
