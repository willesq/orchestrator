import OrchKit
import SwiftUI

/// The Spaces pane's Edit page (SP-053): the advanced editors for the
/// spaces and their bindings, who reads whom, the rules the policy makes,
/// what each harness covers, and where stored sources sit. Edits are saved
/// as a draft; the Review page applies it (D-71/D-78).
struct SpacesEditorView: View {
    let model: SpacesEditorModel
    let enforcement: EnforcementModel
    let navigator: Navigator
    /// Opens the Review page, where a saved draft is applied.
    let openReview: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            EditorHeader(model: model, openReview: openReview)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
                    if let off = model.offNotice {
                        OffNotice(text: off, open: { navigator.showSettings(.spaces) })
                    }
                    EnforcementBanner(model: enforcement, show: { navigator.showSettings(.spaces) })
                    if let error = model.loadError {
                        Label { Text(verbatim: error) } icon: { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                            .textStyle(.supporting)
                    }
                    switch model.tab {
                    case .spaces: SpacesBindingsEditor(model: model)
                    case .access: AccessEditor(model: model)
                    case .rules: RulesEditor(model: model)
                    case .coverage: CoverageEditor(model: model, enforcement: enforcement)
                    case .sources: SourcesEditor(model: model)
                    }
                }
                .padding(Metrics.pagePadding)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .task { await model.load() }
    }
}

/// The editor switch, what is being edited, and Save / Revert / Review.
private struct EditorHeader: View {
    @Bindable var model: SpacesEditorModel
    let openReview: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            // The five editors fit as segments beside the pane; at the
            // companion width they become a menu.
            ViewThatFits(in: .horizontal) {
                picker.pickerStyle(.segmented).fixedSize()
                picker.pickerStyle(.menu).fixedSize()
            }
            FlowLayout(spacing: 8, lineSpacing: 6) {
                Text(verbatim: source).textStyle(.supporting).lineLimit(1)
                Button("Revert") { model.revert() }.disabled(!model.dirty || model.isSaving)
                Button("Save Draft") { Task { await model.save() } }
                    .keyboardShortcut("s", modifiers: .command)
                    .disabled(model.saveBlocker != nil || model.isSaving)
                    .help("Saves a draft revision; nothing is enforced until it is applied on the Review page")
                Button("Review & Apply…", action: openReview)
            }
            if model.dirty, let blocker = model.saveBlocker {
                Label { Text(verbatim: blocker) } icon: { Image(systemName: "xmark.octagon.fill").foregroundStyle(.red) }
                    .textStyle(.caption)
            }
            if let notice = model.notice {
                Text(verbatim: notice).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
            }
            ForEach(model.savedProblems, id: \.self) { p in
                Text(verbatim: "• " + displaySafe(p.message)).font(.caption).foregroundStyle(.red)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var picker: some View {
        Picker("Editor", selection: $model.tab) {
            ForEach(SpacesEditorTab.allCases) { Text($0.title).tag($0) }
        }
        .labelsHidden()
    }

    private var source: String {
        guard let diff = model.diff else { return "" }
        var text: String
        if diff.draftRevision != 0 {
            text = "Editing draft revision \(diff.draftRevision)"
            if !diff.draftByOperator { text += ", saved by an agent session" }
        } else if diff.appliedRevision != 0 {
            text = "Editing applied revision \(diff.appliedRevision)"
        } else {
            text = "No policy yet"
        }
        return model.dirty ? text + " · unsaved changes" : text
    }
}

/// Why nothing is enforced while memory spaces are off, with the way to
/// the switch, which only the operator can use.
private struct OffNotice: View {
    let text: String
    let open: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "power.circle").foregroundStyle(.orange).accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 6) {
                Text(verbatim: text).textStyle(.supporting).fixedSize(horizontal: false, vertical: true)
                Button("Open Spaces Settings", action: open)
                    .help("The memory spaces switch is there; only you can turn it on")
            }
            Spacer(minLength: 0)
        }
        .padding(10)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

// MARK: Spaces

/// Each space with its bindings, kind, gh and Claude folders, terms (count
/// and fingerprint only), and the roots tree.
private struct SpacesBindingsEditor: View {
    let model: SpacesEditorModel
    @State private var newName = ""
    @State private var newKind = "isolated"
    @State private var termsFor: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
            RootsTreeCard(tree: model.rootsTree)
            ForEach(model.spaces) { space in
                SpaceCardEditor(model: model, space: space, editTerms: { termsFor = space.name })
            }
            Card("Add a space") {
                HStack(spacing: 8) {
                    TextField("Name", text: $newName, prompt: Text(verbatim: "acme"))
                        .textFieldStyle(.roundedBorder).labelsHidden()
                    KindPicker(kind: $newKind)
                    Button("Add") {
                        model.addSpace(name: newName, kind: newKind)
                        newName = ""
                    }
                    .disabled(newName.trimmingCharacters(in: .whitespaces).isEmpty)
                }
                Text("Names are 1-32 lowercase letters, digits, - or _, starting with a letter.").textStyle(.caption)
            }
            Card("Sessions under a public root") {
                Picker("Write to", selection: Binding(get: { model.edit.publicRootSessions },
                                                      set: { model.setPublicRootSessions($0) })) {
                    Text("orchd's choice").tag("")
                    ForEach(model.spaces.filter { $0.kind != "public" }) { Text(verbatim: displaySafe($0.name)).tag($0.name) }
                }
                Text("They read only public spaces; what they write goes here. orchd's choice is the only private space, if there is exactly one, else quarantine.")
                    .textStyle(.caption)
            }
        }
        .sheet(isPresented: Binding(get: { termsFor != nil }, set: { if !$0 { termsFor = nil } })) {
            if let space = termsFor { TermsSheet(model: model, space: space) { termsFor = nil } }
        }
    }
}

private struct KindPicker: View {
    @Binding var kind: String

    var body: some View {
        Picker("Kind", selection: $kind) {
            Text("Isolated · job").tag("isolated")
            Text("Private · personal").tag("private")
            Text("Public").tag("public")
            if !SpacesPolicyEdit.kinds.contains(kind) { Text(verbatim: displaySafe(kind)).tag(kind) }
        }
        .labelsHidden()
        .fixedSize()
    }
}

/// One space's editor card.
private struct SpaceCardEditor: View {
    let model: SpacesEditorModel
    let space: EditableSpace
    let editTerms: () -> Void
    @State private var confirmRemove = false

    var body: some View {
        Card(space.name.isEmpty ? "Unnamed space" : displaySafe(space.name)) {
            HStack(spacing: 6) {
                if let c = model.count(space.name) {
                    Chip(text: "\(c.chunks) chunks" + (c.stale > 0 ? " · \(c.stale) stale" : ""),
                         help: "Stored chunks in this space; stale ones are labelled for an older policy")
                }
                Button(role: .destructive) { confirmRemove = true } label: { Image(systemName: "trash") }
                    .buttonStyle(.borderless).help("Remove this space")
            }
        } content: {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 8) {
                    TextField("Name", text: value("name")).textFieldStyle(.roundedBorder).frame(maxWidth: 200)
                    KindPicker(kind: value("kind"))
                }
                LinesField(title: "GitHub owners", prompt: "acme", values: space.githubOrgs) { model.setList("github_orgs", of: space.id, to: $0) }
                LinesField(title: "Folder roots", prompt: "~/code/acme", values: space.roots) { model.setList("roots", of: space.id, to: $0) }
                if space.kind == "public" || !space.publicPaths.isEmpty {
                    LinesField(title: "Public paths", prompt: "~/code/oss/**", values: space.publicPaths) {
                        model.setList("public_paths", of: space.id, to: $0)
                    }
                    Text("Only these folders under the space's roots are public: an exact folder, * within one name, ** for any depth. The rest is quarantined.")
                        .textStyle(.caption)
                }
                LinesField(title: "Web prefixes", prompt: "https://wiki.acme.dev/", values: space.webPrefixes) { model.setList("web_prefixes", of: space.id, to: $0) }
                LinesField(title: "Chat tenants", prompt: "slack:T0123", values: space.chat) { model.setList("chat", of: space.id, to: $0) }
                LabeledContent("gh profile") {
                    TextField("gh profile", text: value("gh_config_dir"), prompt: Text(verbatim: "~/.config/gh-acme")).textFieldStyle(.roundedBorder)
                }
                LabeledContent("Claude Code folder") {
                    TextField("Claude Code folder", text: value("claude_config_dir"), prompt: Text("your own")).textFieldStyle(.roundedBorder)
                }
                TermsRow(model: model, space: space, edit: editTerms, path: value("terms_file"))
            }
        }
        .confirmationDialog("Remove \(displaySafe(space.name))?", isPresented: $confirmRemove, titleVisibility: .visible) {
            Button("Remove", role: .destructive) { model.removeSpace(at: space.id) }
        } message: {
            Text("Its bindings leave the draft and other spaces stop reading it. Content stored there stays until you retag it; nothing changes until the draft is applied.")
        }
    }

    private func value(_ field: String) -> Binding<String> {
        Binding(get: {
            switch field {
            case "name": space.name
            case "kind": space.kind
            case "gh_config_dir": space.ghConfigDir
            case "claude_config_dir": space.claudeConfigDir
            default: space.termsFile
            }
        }, set: { model.setValue(field, of: space.id, to: $0) })
    }
}

/// The terms file path, what orchd says of the file, and Replace Terms…
private struct TermsRow: View {
    let model: SpacesEditorModel
    let space: EditableSpace
    let edit: () -> Void
    let path: Binding<String>

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            LabeledContent("Terms file") {
                TextField("Terms file", text: path, prompt: Text(verbatim: "~/.config/orch/terms/\(space.name).txt"))
                    .textFieldStyle(.roundedBorder)
            }
            let info = model.termsInfo(space.name)
            // orchd writes only the applied revision's file, which a draft
            // may have moved.
            if let applied = info?.termsFile, !applied.isEmpty, applied != space.termsFile {
                Label { Text(verbatim: "Replace Terms writes the applied file, \(displaySafe(applied)), until this draft is applied.") }
                    icon: { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                    .textStyle(.caption)
            }
            HStack(spacing: 8) {
                Text(verbatim: info?.summary ?? (model.termsError ?? "Not applied yet")).textStyle(.caption)
                Spacer(minLength: 8)
                Button("Replace Terms…", action: edit)
                    .disabled(info == nil || info?.termsFile.isEmpty == true)
                    .help("Write the words and patterns that must not leave this space. They are sent to orchd and never shown again.")
            }
        }
    }
}

/// The write-only terms field: the typed text lives only in this sheet's
/// state, is sent once, and is cleared; orchd answers with a count and a
/// fingerprint, never the terms (INV-07).
private struct TermsSheet: View {
    let model: SpacesEditorModel
    let space: String
    let done: () -> Void
    @State private var text = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Replace the terms of \(displaySafe(space))").textStyle(.section)
            Text("One term per line: a word or phrase, or re:<pattern>. Lines starting with # are notes. This replaces the whole file; the current terms are never shown.")
                .textStyle(.supporting).fixedSize(horizontal: false, vertical: true)
            TextEditor(text: $text)
                .font(TextStyle.code.font)
                .frame(minHeight: 140)
                .border(Color.surfaceStroke)
                .autocorrectionDisabled()
            HStack {
                Spacer()
                Button("Cancel") { text = ""; done() }.keyboardShortcut(.cancelAction)
                Button("Replace") {
                    let sent = text
                    Task {
                        if await model.setTerms(space: space, text: sent) { text = ""; done() }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || model.termsSending != nil)
            }
            if let notice = model.notice, model.termsSending == nil {
                Text(verbatim: notice).font(.caption).foregroundStyle(Color.secondaryText)
            }
        }
        .padding(20)
        .frame(minWidth: 380)
        .onDisappear { text = "" }
    }
}

/// A list edited as lines of text, one entry per line.
struct LinesField: View {
    let title: String
    let prompt: String
    let values: [String]
    let commit: ([String]) -> Void
    @State private var text = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).textStyle(.caption)
            TextField(title, text: $text, prompt: Text(verbatim: prompt), axis: .vertical)
                .textFieldStyle(.roundedBorder)
                .font(TextStyle.code.font)
                .lineLimit(1...8)
                .labelsHidden()
        }
        .onAppear { text = values.joined(separator: "\n") }
        .onChange(of: text) { _, new in
            let lines = Self.lines(new)
            if lines != values { commit(lines) }
        }
        .onChange(of: values) { _, new in
            if Self.lines(text) != new { text = new.joined(separator: "\n") }
        }
    }

    static func lines(_ text: String) -> [String] {
        text.split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    }
}

/// Every root of the draft, nested, with its space and why orchd would
/// refuse it.
struct RootsTreeCard: View {
    let tree: RootsTree

    var body: some View {
        Card("Folder roots") {
            if tree.blocksSubmit {
                Chip(text: tree.problemCount == 1 ? "1 problem" : "\(tree.problemCount) problems", tone: .bad, symbol: "xmark.octagon")
            }
        } content: {
            if tree.nodes.isEmpty {
                Text("No space has a folder root.").textStyle(.supporting)
            }
            ForEach(tree.nodes) { node in
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 6) {
                        Image(systemName: node.depth == 0 ? "folder" : "arrow.turn.down.right")
                            .foregroundStyle(Color.secondaryText).accessibilityHidden(true)
                        Text(verbatim: displaySafe(node.root)).textStyle(.code).lineLimit(1).truncationMode(.middle)
                        Chip(text: displaySafe(node.space), tone: node.problems.isEmpty ? kindTone(node.kind) : .bad)
                        Text(verbatim: displaySafe(node.kind)).textStyle(.caption)
                    }
                    ForEach(node.problems, id: \.self) { p in
                        Label { Text(verbatim: p) } icon: { Image(systemName: "xmark.octagon.fill") }
                            .font(.caption).foregroundStyle(.red)
                    }
                }
                .padding(.leading, CGFloat(min(node.depth, 6)) * 14)
            }
            Text("A deeper root decides for what is inside it, and must be at least as strict as every root above it; nothing of another space sits inside a job's root.")
                .textStyle(.caption)
        }
    }
}

func kindTone(_ kind: String) -> Tone {
    switch kind {
    case "public": .good
    case "isolated": .warning
    default: .accent
    }
}
