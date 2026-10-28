import OrchKit
import SwiftUI

// Reusable pieces for any view over orchd's settings providers: a kind's
// items grouped into sections, a row with its toggle and origin, the detail
// of the selected item, and an add form built from the kind's fields. The
// Claude tab is one view built from them. Every string here comes from a
// file another tool owns, so all of it goes through displaySafe.

/// One kind's items, grouped (by event, list, rulebook) into sections.
struct SettingsKindList: View {
    let model: SettingsModel
    let provider: String
    let kind: String
    /// Shown when the kind has no items.
    let emptyText: String
    /// Keeps only items whose name, text or matcher contains it.
    var query = ""

    var body: some View {
        let spec = model.listing?.provider(provider)?.kind(kind)
        let groups = model.groups(provider: provider, kind: kind, matching: query)
        List(selection: Binding(get: { model.selection }, set: { model.selection = $0 })) {
            ForEach(groups) { group in
                Section {
                    ForEach(group.items) { item in
                        SettingRow(model: model, item: item, kind: spec).tag(item.ref)
                    }
                } header: {
                    HStack {
                        Text(verbatim: displaySafe(group.name.isEmpty ? "Other" : group.name)).textStyle(.section)
                        Spacer()
                        let on = group.items.filter(\.isOn).count
                        Text(on == group.items.count ? "\(on) on" : "\(on) of \(group.items.count) on").textStyle(.caption)
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityAddTraits(.isHeader)
                }
            }
        }
        .overlay {
            if model.listing != nil && groups.isEmpty {
                if query.isEmpty {
                    EmptyState(symbol: "tray", title: "Nothing here yet", message: emptyText)
                } else {
                    EmptyState(symbol: "magnifyingglass", title: "No matches", message: "Nothing matches \u{201C}\(query)\u{201D}.")
                }
            }
        }
    }
}

/// An item's on/off switch, shared by every view that lists settings items
/// so the Claude and Safety Net tabs toggle the same thing the same way.
struct SettingToggle: View {
    let model: SettingsModel
    let item: SettingItem
    let kind: SettingKind?
    /// What VoiceOver calls the switch.
    let label: String

    @State private var confirmOff = false

    var body: some View {
        Toggle(label, isOn: Binding(
            get: { item.isOn },
            set: { on in
                // Reason: a security module is the agents' own policy; one
                // stray click should not drop it from every session.
                if !on, item.module?.sensitive == true {
                    confirmOff = true
                } else {
                    Task { await model.setEnabled(item, on) }
                }
            }
        ))
        .confirmationDialog("Turn off \(label)?", isPresented: $confirmOff) {
            Button("Turn Off", role: .destructive) { Task { await model.setEnabled(item, false) } }
        } message: {
            Text("This is part of your agents' security policy. Every new session on this Mac will start without it, until you turn it back on.")
        }
        .labelsHidden()
        .toggleStyle(.switch)
        .controlSize(.mini)
        .disabled(model.pending.contains(item.ref) || !item.canToggle(in: kind))
        .help(item.isOn ? "On. Turn off to take it out of effect." : "Off. Turn on to put it back exactly.")
        .accessibilityLabel(label)
    }
}

/// A row: the toggle, a readable name with the exact text under it, the
/// origin, and any note or error.
struct SettingRow: View {
    let model: SettingsModel
    let item: SettingItem
    let kind: SettingKind?

    var body: some View {
        let display = item.display
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            SettingToggle(model: model, item: item, kind: kind, label: display.title)
                .alignmentGuide(.firstTextBaseline) { $0[VerticalAlignment.center] + 4 }
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(verbatim: display.title)
                        .font(display.literalTitle ? TextStyle.code.font : TextStyle.rowTitle.font)
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .foregroundStyle(item.isOn ? Color.primary : Color.secondaryText)
                    if !display.matcher.isEmpty {
                        Chip(text: display.matcher, help: "Runs only for tools matching \(display.matcher)")
                    }
                }
                if !display.detail.isEmpty && display.detail != display.title {
                    Text(verbatim: display.detail)
                        .textStyle(display.proseDetail ? .supporting : .codeCaption)
                        .lineLimit(1)
                        .truncationMode(.tail)
                        .help(display.detail)
                }
                if let note = item.status.note {
                    Text(note).font(.caption).foregroundStyle(.orange)
                }
                if let error = model.itemErrors[item.ref] {
                    Label { Text(verbatim: displaySafe(error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                        .font(.caption)
                        .foregroundStyle(.red)
                        .textSelection(.enabled)
                }
            }
            Spacer(minLength: 4)
            if model.pending.contains(item.ref) { ProgressView().controlSize(.small) }
            OriginBadge(origin: item.origin)
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .contain)
    }
}

/// Who put an item in its file.
struct OriginBadge: View {
    let origin: SettingOrigin

    var body: some View {
        Chip(text: origin.label, tone: tone, help: origin.help)
    }

    private var tone: Tone {
        switch origin {
        case .orch: .accent
        case .you: .good
        case .found: .neutral
        }
    }
}

/// The selected item: what it is, where it came from, and exactly what is
/// written to its file.
struct SettingDetail: View {
    let model: SettingsModel

    @State private var confirmRemove = false

    var body: some View {
        if let item = model.selected, let module = item.module {
            ModuleDetail(item: item, module: module)
        } else if let item = model.selected {
            let kind = model.kind(of: item.ref)
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    let display = item.display
                    HStack(alignment: .firstTextBaseline) {
                        Text(verbatim: display.title)
                            .font(display.literalTitle ? .headline.monospaced() : TextStyle.title.font)
                            .textSelection(.enabled)
                        Spacer()
                        OriginBadge(origin: item.origin)
                    }
                    if !display.detail.isEmpty && display.detail != display.title {
                        Text(verbatim: display.detail)
                            .textStyle(.code)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                        row("State", item.isOn ? "on" : "off")
                        if let kind, !kind.groupTitle.isEmpty { row(kind.groupTitle, item.group) }
                        row("ID", item.ref.identity)
                        row("Origin", item.origin.help)
                        if let at = item.updatedAt {
                            GridRow {
                                Text("Changed").foregroundStyle(Color.secondaryText)
                                Text(at, format: .relative(presentation: .named))
                            }
                        }
                    }
                    .font(.callout)
                    if let note = item.status.note {
                        Text(note).font(.callout).foregroundStyle(.orange)
                    }
                    Text(kind?.stateInFile == true ? "In the file" : "Written to the file when on")
                        .textStyle(.caption)
                    Text(verbatim: item.prettyPayload)
                        .font(.caption.monospaced())
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(8)
                        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
                    if item.canRemove(in: kind) {
                        Button(item.status == .removed ? "Forget…" : "Remove…", role: .destructive) { confirmRemove = true }
                            .disabled(model.pending.contains(item.ref))
                            .help("Take it out of the file and forget it")
                            .confirmationDialog(item.status == .removed ? "Forget this item?" : "Remove this from its file?",
                                                isPresented: $confirmRemove) {
                                Button("Remove", role: .destructive) { Task { await model.remove(item) } }
                            } message: {
                                Text("Orch forgets it too. Turn it off instead to keep it for later.")
                            }
                    } else if item.origin == .orch {
                        Text("orch install adds this, so it can be turned off but not removed.")
                            .textStyle(.caption)
                    }
                }
                .padding()
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        } else {
            EmptyState(symbol: "sidebar.right", title: "No item selected",
                       message: "Select an item to see its full command and exactly what it writes.")
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(Color.secondaryText)
            Text(verbatim: displaySafe(value)).textSelection(.enabled).lineLimit(3)
        }
    }
}

/// An instruction module: what it is, where it installs, and its full text
/// rendered, so a module can be read here instead of opening its file.
struct ModuleDetail: View {
    let item: SettingItem
    let module: ModulePayload

    @State private var text: NSAttributedString?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 8) {
                HStack(alignment: .firstTextBaseline) {
                    Text(verbatim: displaySafe(module.title)).font(TextStyle.title.font).textSelection(.enabled)
                    Spacer()
                    OriginBadge(origin: item.origin)
                }
                if !module.description.isEmpty {
                    Text(verbatim: displaySafe(module.description)).textStyle(.supporting)
                }
                Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                    GridRow {
                        Text("State").foregroundStyle(Color.secondaryText)
                        Text(item.isOn ? "Loaded by agents on this Mac" : "Off on this Mac")
                    }
                    GridRow {
                        Text("Topic").foregroundStyle(Color.secondaryText)
                        Text(verbatim: displaySafe(item.group))
                    }
                    GridRow {
                        Text("File").foregroundStyle(Color.secondaryText)
                        Text(verbatim: displaySafe(module.path)).textStyle(.codeCaption).textSelection(.enabled)
                            .lineLimit(1).truncationMode(.middle)
                    }
                }
                .font(.callout)
            }
            .padding()
            Divider()
            Group {
                if let text {
                    RichTextView(text: text)
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .task(id: module.body) {
            text = nil
            let made = await MarkdownRenderer().renderInBackground(module.body)
            guard !Task.isCancelled else { return }
            text = made?.text ?? NSAttributedString(string: module.body)
        }
        // Reason: the rendered text has almost no ideal width of its own, so
        // without this the split view collapses the pane to its minimum.
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

/// An add form built from a kind's fields; orchd validates what is sent.
struct SettingAddForm: View {
    let model: SettingsModel
    @State var draft: SettingDraft
    @Environment(\.dismiss) private var dismiss
    @State private var error: String?
    @State private var sending = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Add to \(draft.kind.title)").font(.title3.weight(.semibold))
            Form {
                ForEach(draft.kind.fields) { field in
                    fieldView(field)
                }
            }
            .formStyle(.grouped)
            if let error {
                Label { Text(verbatim: displaySafe(error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                    .foregroundStyle(.red)
                    .font(.callout)
                    .textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Add") {
                    sending = true
                    Task {
                        error = await model.add(draft)
                        sending = false
                        if error == nil { dismiss() }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(sending || draft.problem != nil)
            }
        }
        .padding()
        .frame(minWidth: 480, minHeight: 360)
    }

    @ViewBuilder
    private func fieldView(_ field: SettingField) -> some View {
        let binding = Binding(get: { draft.values[field.name] ?? "" }, set: { draft.values[field.name] = $0 })
        let label = field.required ? field.label + " *" : field.label
        Group {
            switch field.type {
            case .enum:
                Picker(label, selection: binding) {
                    ForEach(field.options, id: \.self) { Text(verbatim: $0).tag($0) }
                }
            case .bool:
                Toggle(label, isOn: Binding(get: { binding.wrappedValue == "true" }, set: { binding.wrappedValue = $0 ? "true" : "false" }))
            case .text:
                VStack(alignment: .leading) {
                    Text(label)
                    TextEditor(text: binding).font(TextStyle.code.font).frame(minHeight: 60)
                }
            case .int, .string:
                // The label stays in the body font; only what is typed is a
                // literal. The example is marked as one, so it is not read as
                // a value already filled in.
                LabeledContent(label) {
                    TextField(label, text: binding,
                              prompt: field.placeholder.isEmpty ? nil : Text(verbatim: "e.g. \(field.placeholder)"))
                        .labelsHidden()
                        .font(TextStyle.code.font)
                        .multilineTextAlignment(.leading)
                }
            }
        }
        .help(field.help)
    }
}
