import OrchKit
import SwiftUI

struct DocsView: View {
    @Bindable var model: DocsModel

    var body: some View {
        AdaptiveSplit(primaryWidth: .init(min: 260, ideal: 320, max: 460), secondaryWidth: .init(min: 380),
                      stacking: .detail(shown: model.selected != nil, backTitle: "Docs",
                                        back: { Task { await model.select(nil) } })) {
            DocList(model: model)
        } secondary: {
            DocDetail(model: model)
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
        .task {
            if model.catalog == nil { await model.load() }
            // A sync already running when the tab opens is followed too.
            if model.sync?.running == true { await model.follow() }
        }
    }
}

private struct DocList: View {
    @Bindable var model: DocsModel

    var body: some View {
        // Grouped once per update rather than once per use.
        let groups = model.groups
        VStack(spacing: 0) {
            SyncStatus(model: model)
            TextField("Filter by path or repo", text: $model.filter)
                .textFieldStyle(.roundedBorder)
                .padding(8)
            if let error = model.lastError {
                ErrorText("orchd: \(error)", symbol: "bolt.slash")
                    .padding(.horizontal, 8)
            }
            List(selection: Binding(get: { model.selected }, set: { file in Task { await model.select(file) } })) {
                ForEach(groups) { group in
                    Section {
                        ForEach(group.files) { file in
                            HStack {
                                Text(verbatim: displaySafe(file.path))
                                    .lineLimit(1)
                                    .truncationMode(.middle)
                                Spacer()
                                Text("\(file.chunks) chunk\(file.chunks == 1 ? "" : "s")")
                                    .textStyle(.caption)
                                    .monospacedDigit()
                            }
                            .padding(.leading, 4)
                            .help("\(displaySafe(file.path)): \(file.chunks) chunk\(file.chunks == 1 ? "" : "s") in memory")
                            .tag(file)
                        }
                    } header: {
                        RepoHeader(group: group)
                    }
                }
            }
            .overlay {
                if model.catalog != nil && groups.isEmpty {
                    if model.filter.isEmpty {
                        EmptyState(symbol: "doc.text.magnifyingglass", title: "No synced docs",
                                   message: "Opt an account or org in under Settings → Docs and its docs show up here.")
                    } else {
                        EmptyState(symbol: "magnifyingglass", title: "No docs match",
                                   actionTitle: "Clear filter", action: { model.filter = "" })
                    }
                }
            }
            Divider()
            HStack {
                Text("\(DocsModel.fileCount(groups)) docs · \(DocsModel.chunkCount(groups)) chunks in memory")
                if model.catalog?.truncated == true {
                    Text("(first \(model.catalog?.files.count ?? 0) shown)")
                }
                Spacer()
            }
            .textStyle(.caption)
            .padding(.horizontal, 10)
            .padding(.vertical, 6)
        }
    }
}

private struct RepoHeader: View {
    let group: DocsModel.RepoGroup

    var body: some View {
        // The repo is the heading; its files sit under it in the body font.
        HStack(alignment: .firstTextBaseline) {
            Image(systemName: "book.closed").foregroundStyle(Color.secondaryText).accessibilityHidden(true)
            Text(verbatim: displaySafe("\(group.owner)/\(group.repo)"))
                .font(.headline)
                .foregroundStyle(Color.primary)
            if let info = group.info, info.failed {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
                    .help(displaySafe("Last sync failed: \(info.error)"))
                    .accessibilityLabel("Last sync failed")
            }
            Spacer()
            if let info = group.info, let synced = info.lastSynced {
                Text("synced \(synced, format: .relative(presentation: .named))").textStyle(.caption)
            }
        }
        .padding(.top, 6)
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isHeader)
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
                // The search bar takes one line until it has something to show.
                let searchIdle = model.search.result == nil && !model.search.isSearching && model.search.lastError == nil
                ScopedSearch(search: model.search, scope: Binding(get: { model.scope }, set: { model.setScope($0) }),
                             onOpen: { uri in openDoc(uri) })
                    .frame(height: searchIdle ? 56 : 240)
                Divider()
                DocumentText(model: model)
                    .frame(minHeight: 200, maxHeight: .infinity)
            }
        } else {
            EmptyState(symbol: "doc.text", title: "No doc selected",
                       message: "Choose a doc to read it as memory holds it, search it, or open it on GitHub.")
        }
    }

    private func header(_ file: DocFile) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(verbatim: displaySafe(file.path)).textStyle(.title).textSelection(.enabled)
            HStack(spacing: 4) {
                Text(verbatim: displaySafe("\(file.owner)/\(file.repo)"))
                if let updated = file.updatedAt {
                    Text("· indexed \(updated, format: .relative(presentation: .named))")
                }
            }
            .textStyle(.supporting)
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
                // An address with control characters could run as more than one
                // command when pasted into a terminal.
                .disabled(!isPlain(file.sourceURI))
                .help(displaySafe(file.sourceURI))
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

    enum Mode: String, CaseIterable, Identifiable {
        case rendered = "Rendered", source = "Source"
        var id: String { rawValue }
    }

    @AppStorage("docsReaderMode") private var mode: Mode = .rendered

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("Rebuilt from memory").textStyle(.section)
                if let doc = model.document {
                    Text("\(doc.chunks) chunk\(doc.chunks == 1 ? "" : "s")").textStyle(.caption)
                }
                Spacer()
                if model.document?.isMarkdown == true, model.document?.oversize == false {
                    Picker("Show", selection: $mode) {
                        ForEach(Mode.allCases) { Text($0.rawValue).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    .fixedSize()
                    .controlSize(.small)
                }
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            if model.isLoadingDocument {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let error = model.documentError {
                ErrorText(error, symbol: "exclamationmark.triangle", font: .body).padding(12)
                Spacer()
            } else if let doc = model.document {
                RebuildCallout(malformed: doc.malformed)
                    .padding(.horizontal, 12)
                    .padding(.bottom, 8)
                if doc.oversize {
                    Text("This doc is too large to show here. Open it on GitHub instead.")
                        .foregroundStyle(Color.secondaryText)
                        .padding(12)
                    Spacer()
                } else {
                    DocumentBody(doc: doc, rendered: doc.isMarkdown && mode == .rendered)
                }
            }
        }
    }
}

/// The note that a rebuilt doc is close to, but not exactly, the original.
private struct RebuildCallout: View {
    let malformed: Bool

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: malformed ? "exclamationmark.triangle.fill" : "info.circle.fill")
                .foregroundStyle(malformed ? Color.orange : Color.accentColor)
            Text(malformed
                ? "Rebuilt from memory, but some chunks were out of order, so this is a best effort. Open it on GitHub for the original."
                : "Rebuilt from the chunks agents retrieve. It is close to the original but not exact: a table or code block split across chunks may look slightly off.")
                .font(.callout)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background((malformed ? Color.orange : Color.accentColor).opacity(0.1), in: RoundedRectangle(cornerRadius: 8))
    }
}

/// The doc's text, rendered as markdown or shown as its source. Rendering
/// runs off the main thread, so a long doc does not stall the window.
private struct DocumentBody: View {
    let doc: RebuiltDocument
    let rendered: Bool

    @State private var text: NSAttributedString?

    var body: some View {
        Group {
            if let text {
                RichTextView(text: text)
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: "\(doc.sourceURI)|\(rendered)") {
            text = nil
            // Markdown too deeply nested to render safely is shown as its source.
            let made = rendered ? await MarkdownRenderer(baseURL: gitHubBlobURL(doc.webURL)).renderInBackground(doc.text) : nil
            guard !Task.isCancelled else { return }
            text = made?.text ?? plainSource(doc.text)
        }
    }
}

/// A doc's text as its source: plain monospaced, nothing interpreted.
private func plainSource(_ text: String) -> NSAttributedString {
    NSAttributedString(string: text, attributes: [
        .font: NSFont.monospacedSystemFont(ofSize: NSFont.systemFontSize, weight: .regular),
        .foregroundColor: NSColor.labelColor,
    ])
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
                ErrorText(error)
            }
            if search.isSearching {
                ProgressView().controlSize(.small)
            } else if let result = search.result {
                if result.hits.isEmpty {
                    Text("No matches.").font(.caption).foregroundStyle(Color.secondaryText)
                }
                List(Array(result.hits.enumerated()), id: \.offset) { _, hit in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack {
                            Text(String(format: "%.3f", hit.score)).font(.caption.monospacedDigit()).foregroundStyle(Color.secondaryText)
                            Button { onOpen(hit.sourceURI) } label: {
                                Text(verbatim: displaySafe(hit.sourceURI)).font(.caption).lineLimit(1).truncationMode(.middle)
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

/// Read-only, selectable text in an NSTextView, which lays out only what is
/// visible: a multi-megabyte doc in a SwiftUI Text would block the main thread
/// while every line is laid out. Link clicks are checked again before anything
/// opens.
struct RichTextView: NSViewRepresentable {
    let text: NSAttributedString

    func makeCoordinator() -> Coordinator { Coordinator() }


    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSTextView.scrollableTextView()
        if let view = scroll.documentView as? NSTextView {
            view.isEditable = false
            view.isSelectable = true
            view.usesFindBar = true
            view.drawsBackground = true
            view.backgroundColor = .textBackgroundColor
            view.textContainerInset = NSSize(width: 12, height: 10)
            view.layoutManager?.allowsNonContiguousLayout = true
            view.delegate = context.coordinator
            view.linkTextAttributes = [.foregroundColor: NSColor.linkColor, .underlineStyle: NSUnderlineStyle.single.rawValue,
                                       .cursor: NSCursor.pointingHand]
        }
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        // Identity, not equality: comparing a large doc on every update would
        // cost the main thread for nothing.
        guard let view = scroll.documentView as? NSTextView, context.coordinator.shown !== text else { return }
        context.coordinator.shown = text
        view.textStorage?.setAttributedString(text)
        view.scrollToBeginningOfDocument(nil)
    }

    final class Coordinator: NSObject, NSTextViewDelegate {
        var shown: NSAttributedString?

        func textView(_ textView: NSTextView, clickedOnLink link: Any, at charIndex: Int) -> Bool {
            let raw = (link as? URL)?.absoluteString ?? (link as? String) ?? ""
            if let url = MarkdownRenderer.linkURL(raw, base: nil) {
                NSWorkspace.shared.open(url)
            }
            // Handled either way: a link that fails the check opens nothing.
            return true
        }
    }
}

/// Sync progress, and what needs attention after a sync failed: the failing
/// repos with their causes, and Sync now.
private struct SyncStatus: View {
    let model: DocsModel
    @State private var showFailures = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let sync = model.sync, sync.running {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text(verbatim: sync.total > 0
                        ? "Syncing \(displaySafe(sync.owner)): \(sync.done) of \(sync.total) repos"
                        : "Syncing \(displaySafe(sync.owner))…")
                        .font(.callout)
                    Spacer()
                }
            } else if let catalog = model.catalog, catalog.needsAttention {
                attention(catalog)
            }
            if let refused = model.syncError {
                Label(displaySafe(refused), systemImage: "hand.raised").font(.caption).foregroundStyle(Color.secondaryText)
            }
        }
        .padding(.horizontal, 8)
        .padding(.top, 8)
    }

    /// A headline, one line on the most common cause, and Sync now; the
    /// full reasons, grouped by cause, stay behind the disclosure.
    @ViewBuilder private func attention(_ catalog: DocCatalog) -> some View {
        let failing = catalog.failingRepos
        VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline) {
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange).accessibilityHidden(true)
                Text(failing.isEmpty ? "The last docs sync needs attention" : "\(failing.count) repo\(failing.count == 1 ? "" : "s") failed to sync")
                    .font(.callout.weight(.semibold))
                Spacer()
                Button("Sync now") { Task { await model.syncNow() } }
                    .controlSize(.small)
                    .disabled(model.isFollowingSync)
                    .help("Ask orchd to sync now; it runs one sync at a time")
            }
            if let cause = catalog.commonCause {
                Text(verbatim: displaySafe(cause))
                    .textStyle(.caption)
                    .lineLimit(1)
                    .truncationMode(.tail)
                    .help(displaySafe(cause))
            }
            DisclosureGroup(isExpanded: $showFailures) {
                // Capped and scrolling, so a long list cannot push the
                // window's content out of view.
                ScrollView {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(catalog.failingOwners) { owner in
                            Text(verbatim: displaySafe(owner.error)).textStyle(.caption)
                        }
                        ForEach(catalog.failureCauses.prefix(20)) { group in
                            VStack(alignment: .leading, spacing: 2) {
                                Text(verbatim: displaySafe(group.cause)).font(.caption.weight(.semibold))
                                Text(verbatim: displaySafe(group.repos.prefix(50).joined(separator: ", ")))
                                    .textStyle(.caption)
                            }
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .textSelection(.enabled)
                }
                .frame(maxHeight: 200)
            } label: {
                Text(failing.isEmpty ? "Show details" : "Show the failing repos").font(.caption)
            }
        }
        .padding(10)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: Metrics.cardRadius, style: .continuous))
    }
}
