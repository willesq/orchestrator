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

    var body: some View {
        let spec = model.listing?.provider(provider)?.kind(kind)
        let groups = model.groups(provider: provider, kind: kind)
        List(selection: Binding(get: { model.selection }, set: { model.selection = $0 })) {
            ForEach(groups) { group in
                Section {
                    ForEach(group.items) { item in
                        SettingRow(model: model, item: item, kind: spec).tag(item.ref)
                    }
                } header: {
                    HStack {
                        Text(verbatim: displaySafe(group.name.isEmpty ? "Other" : group.name)).font(.headline)
                        Spacer()
                        let on = group.items.filter(\.isOn).count
                        Text("\(on) of \(group.items.count) on").foregroundStyle(.secondary)
                    }
                }
            }
        }
        .overlay {
            if model.listing != nil && groups.isEmpty {
                Text(emptyText).multilineTextAlignment(.center).foregroundStyle(.secondary).padding()
            }
        }
    }
}

/// A row: the toggle, the summary, the origin, and any note or error.
struct SettingRow: View {
    let model: SettingsModel
    let item: SettingItem
    let kind: SettingKind?

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Toggle("", isOn: Binding(
                get: { item.isOn },
                set: { on in Task { await model.setEnabled(item, on) } }
            ))
            .labelsHidden()
            .toggleStyle(.switch)
            .controlSize(.mini)
            .disabled(model.pending.contains(item.ref) || !item.canToggle(in: kind))
            .help(item.isOn ? "On. Turn off to take it out of effect." : "Off. Turn on to put it back exactly.")
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: displaySafe(item.summary.isEmpty ? item.ref.identity : item.summary))
                    .font(.callout.monospaced())
                    .lineLimit(2)
                    .truncationMode(.middle)
                    .foregroundStyle(item.isOn ? .primary : .secondary)
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
        .padding(.vertical, 1)
    }
}

/// Who put an item in its file.
struct OriginBadge: View {
    let origin: SettingOrigin

    var body: some View {
        Text(origin.label)
            .font(.caption2.weight(.semibold))
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(color.opacity(0.16), in: Capsule())
            .foregroundStyle(color)
            .help(origin.help)
    }

    private var color: Color {
        switch origin {
        case .orch: .blue
        case .you: .green
        case .found: .secondary
        }
    }
}

/// The selected item: what it is, where it came from, and exactly what is
/// written to its file.
struct SettingDetail: View {
    let model: SettingsModel

    @State private var confirmRemove = false

    var body: some View {
        if let item = model.selected {
            let kind = model.kind(of: item.ref)
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    HStack(alignment: .firstTextBaseline) {
                        Text(verbatim: displaySafe(item.summary.isEmpty ? item.ref.identity : item.summary))
                            .font(.headline.monospaced())
                            .textSelection(.enabled)
                        Spacer()
                        OriginBadge(origin: item.origin)
                    }
                    Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                        row("State", item.isOn ? "on" : "off")
                        if let kind, !kind.groupTitle.isEmpty { row(kind.groupTitle, item.group) }
                        row("ID", item.ref.identity)
                        row("Origin", item.origin.help)
                        if let at = item.updatedAt {
                            GridRow {
                                Text("Changed").foregroundStyle(.secondary)
                                Text(at, format: .relative(presentation: .named))
                            }
                        }
                    }
                    .font(.callout)
                    if let note = item.status.note {
                        Text(note).font(.callout).foregroundStyle(.orange)
                    }
                    Text(kind?.stateInFile == true ? "In the file" : "Written to the file when on")
                        .font(.caption).foregroundStyle(.secondary)
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
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                .padding()
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        } else {
            Text("Select an item to see exactly what it writes.")
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary)
            Text(verbatim: displaySafe(value)).textSelection(.enabled).lineLimit(3)
        }
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
                    TextEditor(text: binding).font(.body.monospaced()).frame(minHeight: 60)
                }
            case .int, .string:
                TextField(label, text: binding, prompt: field.placeholder.isEmpty ? nil : Text(verbatim: field.placeholder))
                    .font(.body.monospaced())
            }
        }
        .help(field.help)
    }
}
