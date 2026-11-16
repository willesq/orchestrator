import AppKit
import OrchKit
import SwiftUI

// A hit as a result row and in the inspector. Every field is stored,
// untrusted data: it is made display-safe and shown verbatim, never as
// Markdown, and an address is opened only after its scheme is checked.

/// One result: its source with the repo, type, section, dates and score.
struct HitRow: View {
    /// The rank in a ranked search; nil in Browse, which is unranked.
    let rank: Int?
    let hit: SearchHit

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                if let rank {
                    Text("\(rank)").textStyle(.caption).monospacedDigit().frame(minWidth: 16, alignment: .trailing)
                }
                Text(verbatim: hit.displayTitle).font(.body.weight(.medium)).lineLimit(1)
                if !hit.sourceType.isEmpty { Chip(text: displaySafe(hit.sourceType)).fixedSize() }
                Spacer()
                if rank != nil {
                    Text(String(format: "%.2f", hit.score))
                        .textStyle(.caption)
                        .monospacedDigit()
                        .help(hit.scoreHelp)
                } else if let index = hit.chunkIndex {
                    Text("#\(index)").textStyle(.caption).monospacedDigit().help("The chunk's place in its source")
                }
            }
            Group {
                if !hit.heading.isEmpty {
                    Label { Text(verbatim: displaySafe(hit.heading)).lineLimit(1) } icon: { Image(systemName: "number") }
                        .textStyle(.caption)
                        .help("The section the chunk came from")
                }
                HitDates(hit: hit)
                // Indexed content is untrusted: plain Text, never Markdown.
                Text(verbatim: displaySafeLines(hit.preview))
                    .font(.callout)
                    .lineLimit(3)
            }
            .padding(.leading, rank == nil ? 0 : 24)
        }
        .padding(.vertical, 3)
    }
}

/// "Happened 3 Nov 2026 · Stored 5 Nov 2026", each labelled, so the two
/// dates are never confused.
struct HitDates: View {
    let hit: SearchHit

    var body: some View {
        HStack(spacing: 4) {
            if let at = hit.startedAt {
                Text("Happened \(at, format: .dateTime.year().month().day())")
                    .help("When what it describes happened")
            }
            if hit.startedAt != nil && hit.ingestedAt != nil { Text("·") }
            if let at = hit.ingestedAt {
                Text("Stored \(at, format: .dateTime.year().month().day())").help("When it was stored in memory")
            }
        }
        .textStyle(.caption)
    }
}

/// What the inspector can ask of the pane; a nil action hides its button.
struct HitActions {
    var openSource: ((String) -> Void)?
    var searchWithin: ((String) -> Void)?
    var browseWithin: ((String) -> Void)?
}

/// Every field of one hit, with copy, open and narrowing actions.
struct HitInspector: View {
    let hit: SearchHit
    let actions: HitActions
    /// Whether the hit came from a ranked search, so its scores mean something.
    var ranked = true

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text(verbatim: hit.displayTitle).textStyle(.title).textSelection(.enabled)
                buttons
                Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 12, verticalSpacing: 6) {
                    field("Source") {
                        Text(verbatim: displaySafe(hit.sourceURI)).textStyle(.code).textSelection(.enabled)
                    }
                    text("Heading", hit.heading)
                    text("Type", hit.sourceType)
                    text("Scope", hit.scope)
                    text("Collection", hit.collection)
                    if !hit.space.isEmpty { text("Space", hit.space) }
                    text("Trust", hit.trust)
                    text("Tenant", hit.tenant)
                    field("Tags") {
                        if hit.tags.isEmpty {
                            Text("—").textStyle(.supporting)
                        } else {
                            FlowLayout(spacing: 4) {
                                ForEach(Array(hit.tags.enumerated()), id: \.offset) { Chip(text: displaySafe($1)).fixedSize() }
                            }
                        }
                    }
                    date("Happened", hit.startedAt)
                    date("Ended", hit.endedAt)
                    date("Stored", hit.ingestedAt)
                    if ranked {
                        field("Relevance") {
                            Text(String(format: "%.4f", hit.score)).monospacedDigit().help(hit.scoreHelp)
                        }
                        text("Keyword", hit.keywordScore > 0 ? String(format: "%.4f", hit.keywordScore) : "0 (matched by meaning)")
                        text("Exact", hit.exact ? "Yes: contains a code identifier from the query" : "No")
                    }
                    field("Point ID") { Text(verbatim: displaySafe(hit.pointID.isEmpty ? "—" : hit.pointID)).textStyle(.code).textSelection(.enabled) }
                    text("Chunk", hit.chunkIndex.map { "#\($0)" } ?? "")
                }
                .font(.callout)
                Divider()
                Text("Text").textStyle(.section)
                // Indexed content is untrusted: plain Text, never Markdown.
                Text(verbatim: displaySafeLines(hit.text))
                    .font(.callout)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(14)
        }
    }

    private var buttons: some View {
        FlowLayout(spacing: 8) {
            Button {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(hit.json, forType: .string)
            } label: { Label("Copy as JSON", systemImage: "curlybraces") }
                .help("Copy every field of this result")
            if let open = actions.openSource {
                Button { open(hit.sourceURI) } label: { Label("Open whole source", systemImage: "doc.text") }
                    .help("Read the whole source, rebuilt from its chunks")
            }
            if let within = actions.searchWithin {
                Button { within(hit.sourceURI) } label: { Label("Search within", systemImage: "magnifyingglass") }
                    .help("Search only this source")
            }
            if let within = actions.browseWithin {
                Button { within(hit.sourceURI) } label: { Label("Browse source", systemImage: "list.bullet") }
                    .help("List every chunk of this source")
            }
            switch hit.sourceLink {
            case .file(let url)?:
                Button { reveal(url) } label: { Label("Reveal in Finder", systemImage: "folder") }
            case .web(let url)?:
                Button { open(url) } label: { Label("Open", systemImage: "arrow.up.right.square") }
                    .help(url.absoluteString)
            case nil:
                EmptyView()
            }
        }
        .controlSize(.small)
    }

    /// Checked again at the click, so only a file:// path is revealed.
    private func reveal(_ url: URL) {
        guard case .file(let checked)? = SourceLink(uri: url.absoluteString) else { return }
        NSWorkspace.shared.activateFileViewerSelecting([checked])
    }

    /// Checked again at the click, so only a plain http(s) page opens.
    private func open(_ url: URL) {
        guard case .web(let checked)? = SourceLink(uri: url.absoluteString) else { return }
        NSWorkspace.shared.open(checked)
    }

    private func field(_ label: String, @ViewBuilder value: () -> some View) -> some View {
        GridRow {
            Text(label).textStyle(.caption).gridColumnAlignment(.trailing)
            value()
        }
    }

    private func text(_ label: String, _ value: String) -> some View {
        field(label) {
            Text(verbatim: value.isEmpty ? "—" : displaySafe(value))
                .foregroundStyle(value.isEmpty ? Color.secondaryText : .primary)
                .textSelection(.enabled)
        }
    }

    private func date(_ label: String, _ value: Date?) -> some View {
        field(label) {
            if let value {
                Text(value, format: .dateTime.year().month().day().hour().minute()).textSelection(.enabled)
            } else {
                Text("—").foregroundStyle(Color.secondaryText)
            }
        }
    }
}

/// A hit's whole source, rebuilt from its chunks, shown as plain text.
struct SourceDocumentSheet: View {
    let model: SourceDocumentModel
    @Environment(\.dismiss) private var dismiss
    @State private var text: NSAttributedString?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Rebuilt from memory").textStyle(.section)
                    Text(verbatim: displaySafe(model.uri ?? "")).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
                        .textSelection(.enabled)
                }
                Spacer()
                if let doc = model.document {
                    Text("\(doc.chunks) chunk\(doc.chunks == 1 ? "" : "s")").textStyle(.caption)
                }
                Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
            }
            .padding(12)
            Divider()
            if model.isLoading {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let error = model.lastError {
                Label { Text(verbatim: "Could not open the source: \(error)") } icon: {
                    Image(systemName: "exclamationmark.triangle")
                }
                .foregroundStyle(.red)
                .padding(12)
                Spacer()
            } else if let doc = model.document {
                Text(doc.malformed
                    ? "Some chunks were out of order, so this is a best effort."
                    : "Close to the original but not exact: a table or code block split across chunks may look slightly off.")
                    .textStyle(.caption)
                    .padding(12)
                if doc.oversize {
                    Text("This source is too large to show here.").foregroundStyle(Color.secondaryText).padding(12)
                    Spacer()
                } else if let text {
                    RichTextView(text: text)
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .frame(minWidth: 560, idealWidth: 720, minHeight: 420, idealHeight: 600)
        .task(id: model.document?.sourceURI) {
            text = nil
            guard let raw = model.document?.text else { return }
            // Stored text is shown as its source, never rendered; escaping
            // control characters in a long source runs off the main thread.
            let safe = await Task.detached { displaySafeLines(raw) }.value
            guard !Task.isCancelled else { return }
            text = plainSource(safe)
        }
    }
}
