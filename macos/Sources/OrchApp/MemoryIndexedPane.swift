import OrchKit
import SwiftUI

/// Memory → Indexed: the stored sources the filters match, by type and
/// collection, up to a limit or all of them. A selected source opens whole,
/// or narrows Browse or Search to it.
struct SourcesPane: View {
    @Bindable var model: SourcesModel
    let collections: CollectionsModel?
    let actions: HitActions

    @State private var selection: SourceSummary.ID?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            MemoryFilterBar(
                text: $model.filterText, prompt: "Filters, e.g. uri:session:// repo:orchestrator since:7d",
                submitTitle: "List", submit: { Task { await model.refresh() } }, collections: collections,
                collection: $model.collections.single, sourceType: $model.typeFilter, types: model.knownTypes,
                pageSize: $model.limit, pageSizes: SourcesQuery.limitChoices,
                chips: model.parsed.chips, notes: model.parsed.notes,
                remove: { chip in Task { await model.remove(chip) } },
                syntaxHelp: MemoryFilterSyntax.indexed, pageTitle: "Limit", pageLabel: SourcesQuery.label(forLimit:)
            )
            .padding(12)
            // The menus list again at once; the field waits for List.
            .onChange(of: model.collections) { Task { await model.refresh() } }
            .onChange(of: model.typeFilter) { Task { await model.refresh() } }
            .onChange(of: model.limit) { Task { await model.refresh() } }
            HStack {
                if let summary = model.summary { Text(verbatim: summary).textStyle(.caption) }
                Spacer()
                Button { Task { await model.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                    .help("Reload the list")
                    .accessibilityLabel("Reload")
                    .buttonStyle(.borderless)
            }
            .controlSize(.small)
            .padding(.horizontal, 12)
            .padding(.bottom, 6)
            if let error = model.lastError {
                Text(verbatim: "Could not list sources: \(displaySafe(error))").foregroundStyle(.red)
                    .padding(.horizontal, 12).padding(.bottom, 6)
            }
            Divider()
            Table(model.list?.sources ?? [], selection: $selection) {
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
            .contextMenu(forSelectionType: SourceSummary.ID.self) { uris in
                if let uri = uris.first { sourceButtons(uri) }
            }
            if let uri = selection {
                Divider()
                HStack(spacing: 8) {
                    Text(verbatim: displaySafe(uri)).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
                    Spacer()
                    sourceButtons(uri)
                }
                .controlSize(.small)
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
            }
        }
        .task { if model.list == nil { await model.refresh() } }
        .onChange(of: model.answered) { selection = nil }
    }

    /// What a source offers: read it whole, or narrow Browse or Search to it.
    @ViewBuilder private func sourceButtons(_ uri: String) -> some View {
        if let open = actions.openSource {
            Button { open(uri) } label: { Label("Open whole source", systemImage: "doc.text") }
                .help("Read the whole source, rebuilt from its chunks")
        }
        if let browse = actions.browseWithin {
            Button { browse(uri) } label: { Label("Browse this source", systemImage: "list.bullet") }
                .help("List every chunk of this source in Browse")
        }
        if let search = actions.searchWithin {
            Button { search(uri) } label: { Label("Search within this source", systemImage: "magnifyingglass") }
                .help("Search only this source")
        }
    }
}
