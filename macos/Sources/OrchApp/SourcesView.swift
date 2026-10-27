import OrchKit
import SwiftUI

struct SourcesView: View {
    let model: RegisteredSourcesModel

    @State private var editing: Editing?

    /// The editor sheet: a new source, or an existing one.
    struct Editing: Identifiable {
        let id = UUID()
        let source: RegisteredSource?
    }

    var body: some View {
        Group {
            if model.loaded && model.sources.isEmpty {
                // No table, headers or detail pane behind an empty list: just
                // what is missing and the way to add one.
                VStack(spacing: 0) {
                    EmptyState(symbol: "tray.and.arrow.down", title: "No sources yet",
                               message: "Add a web page, a local repo or an owner folder, and orch keeps it in memory, fetching it again on a schedule.",
                               actionTitle: "Add source", action: { editing = Editing(source: nil) })
                    if let error = model.lastError {
                        Label(error, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.red).padding(8)
                    }
                }
            } else if !model.loaded && model.lastError == nil {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                HSplitView {
                    SourceTable(model: model)
                        .frame(minWidth: 380, idealWidth: 520, maxWidth: .infinity)
                    SourceDetail(model: model, onEdit: { editing = Editing(source: $0) })
                        .frame(minWidth: 320, idealWidth: 380, maxWidth: 560, maxHeight: .infinity)
                }
            }
        }
        .navigationTitle("Sources")
        .toolbar {
            ToolbarItem {
                Button { editing = Editing(source: nil) } label: { Label("Add source", systemImage: "plus") }
                    .help("Register a web page, local repo or owner folder to keep in memory")
            }
            ToolbarItem {
                Button { Task { await model.refresh(nil) } } label: { Label("Refresh all", systemImage: "arrow.clockwise") }
                    .disabled(model.busy || model.sources.isEmpty)
                    .help("Fetch every source again now")
            }
        }
        .sheet(item: $editing) { e in
            SourceEditor(model: model, original: e.source)
        }
        .task { await model.load() }
    }
}

private struct SourceTable: View {
    let model: RegisteredSourcesModel

    var body: some View {
        VStack(spacing: 0) {
            Table(model.sources, selection: Binding(get: { model.selectedID }, set: { model.select($0) })) {
                TableColumn("Source") { s in
                    Label { Text(verbatim: displaySafe(s.uri)).lineLimit(1).truncationMode(.middle).help(displaySafe(s.uri)) } icon: {
                        Image(systemName: symbol(s.kind))
                    }
                }
                TableColumn("Status") { s in StatusChip(status: s.status) }
                    .width(70)
                TableColumn("Refresh") { s in Text(refreshLabel(s.refreshSeconds)).foregroundStyle(Color.secondaryText) }
                    .width(90)
                TableColumn("Fetched") { s in
                    if let at = s.lastFetched {
                        Text(at, format: .relative(presentation: .named)).foregroundStyle(Color.secondaryText)
                    } else {
                        Text("due").foregroundStyle(Color.secondaryText)
                    }
                }
                .width(100)
            }
            if model.lastError != nil || model.message != nil {
                Divider()
                VStack(alignment: .leading, spacing: 4) {
                    if let error = model.lastError {
                        Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red)
                    }
                    if let message = model.message {
                        Text(verbatim: message).foregroundStyle(Color.secondaryText)
                    }
                }
                .font(.caption)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(8)
            }
        }
    }
}

private struct SourceDetail: View {
    let model: RegisteredSourcesModel
    let onEdit: (RegisteredSource) -> Void

    @State private var confirmRemove = false

    var body: some View {
        if let source = model.selected {
            VStack(alignment: .leading, spacing: 0) {
                VStack(alignment: .leading, spacing: 8) {
                    Text(verbatim: displaySafe(source.uri)).font(.title3.weight(.semibold)).textSelection(.enabled).lineLimit(3)
                    Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                        row("Kind", source.kind?.label ?? "unknown")
                        row("Status", source.status)
                        row("Refresh", refreshLabel(source.refreshSeconds))
                        if !source.scope.isEmpty { row("Scope", source.scope) }
                        if !source.tags.isEmpty { row("Tags", source.tags.joined(separator: ", ")) }
                    }
                    .font(.callout)
                    HStack {
                        if let url = webSourceURL(source.webURL) {
                            Link(destination: url) { Label("Open", systemImage: "arrow.up.right.square") }
                                .help(url.absoluteString)
                        }
                        Button("Edit…") { onEdit(source) }
                        Button("Refresh now") { Task { await model.refresh(source) } }
                        Button("Remove…", role: .destructive) { confirmRemove = true }
                    }
                    .controlSize(.small)
                    .disabled(model.busy)
                    .confirmationDialog("Remove \(source.uri)?", isPresented: $confirmRemove) {
                        Button("Remove, keep what it indexed") { Task { await model.remove(source, purge: false) } }
                        Button("Remove and delete what it indexed", role: .destructive) {
                            Task { await model.remove(source, purge: true) }
                        }
                    } message: {
                        Text("Deleting its indexed content is permanent. Agents will no longer find it.")
                    }
                }
                .padding(12)
                Divider()
                if source.isSearchable {
                    ScopedSearch<NoScope>(search: model.search)
                } else {
                    Text("This source can't be searched on its own.").foregroundStyle(Color.secondaryText).padding(12)
                }
                Spacer(minLength: 0)
            }
        } else {
            Text("Choose a source to see its details, refresh it or search within it.")
                .foregroundStyle(Color.secondaryText)
                .multilineTextAlignment(.center)
                .padding()
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(Color.secondaryText)
            Text(verbatim: value).textSelection(.enabled)
        }
    }
}

private struct SourceEditor: View {
    let model: RegisteredSourcesModel
    let original: RegisteredSource?

    @Environment(\.dismiss) private var dismiss
    @State private var draft = SourceDraft()

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(original == nil ? "Add a source" : "Edit source").font(.title3.weight(.semibold))
            Form {
                Picker("Kind", selection: $draft.kind) {
                    ForEach(SourceKind.allCases) { Text($0.label).tag($0) }
                }
                TextField("Address", text: $draft.uri, prompt: Text(draft.kind.placeholder))
                Picker("Refresh", selection: $draft.refreshSeconds) {
                    ForEach(SourceDraft.refreshChoices, id: \.seconds) { Text($0.label).tag($0.seconds) }
                    if !SourceDraft.refreshChoices.contains(where: { $0.seconds == draft.refreshSeconds }) {
                        Text(refreshLabel(draft.refreshSeconds)).tag(draft.refreshSeconds)
                    }
                }
                TextField("Scope", text: $draft.scope, prompt: Text("optional"))
                TextField("Tags", text: $draft.tags, prompt: Text("optional, comma-separated"))
            }
            if let problem = draft.problem, !draft.trimmedURI.isEmpty {
                Text(problem).font(.caption).foregroundStyle(.red)
            }
            if let error = model.lastError {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button(original == nil ? "Add" : "Save") {
                    Task {
                        if await model.save(draft, editing: original) { dismiss() }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(draft.problem != nil || model.busy)
            }
        }
        .padding(20)
        .frame(width: 460)
        .onAppear { if let original { draft = original.draft } }
    }
}

private struct StatusChip: View {
    let status: String

    var body: some View {
        Chip(text: displaySafe(status), tone: tone)
    }

    private var tone: Tone {
        switch status {
        case "ok": .good
        case "missing": .warning
        case "error": .bad
        default: .neutral
        }
    }
}

private func symbol(_ kind: SourceKind?) -> String {
    switch kind {
    case .web: "globe"
    case .repo: "folder"
    case .owner: "person.crop.square"
    case nil: "questionmark.square"
    }
}

func refreshLabel(_ seconds: Int64) -> String {
    if let choice = SourceDraft.refreshChoices.first(where: { $0.seconds == seconds }) { return choice.label }
    return "Every \(formatUptime(seconds))"
}
