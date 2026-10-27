import OrchKit
import SwiftUI

/// The Claude tab: Claude Code's hooks, permission rules and plugins from
/// ~/.claude/settings.json, and cc-safety-net's rules, each with a toggle.
/// A change is written to the file at once and kept in this machine's
/// orch.db; edits made to the files elsewhere show up within seconds.
struct ClaudeView: View {
    let model: SettingsModel

    /// One section of the tab: a provider's kind.
    enum Section: String, CaseIterable, Identifiable {
        case hooks, permissions, plugins, rules

        var id: String { rawValue }

        var title: String {
            switch self {
            case .hooks: "Hooks"
            case .permissions: "Permissions"
            case .plugins: "Plugins"
            case .rules: "Safety Net rules"
            }
        }

        var provider: String { self == .rules ? "cc-safety-net" : "claude-code" }

        var kind: String {
            switch self {
            case .hooks: "hook"
            case .permissions: "permission"
            case .plugins: "plugin"
            case .rules: "rule"
            }
        }

        var emptyText: String {
            switch self {
            case .hooks: "No hooks in ~/.claude/settings.json."
            case .permissions: "No permission rules in ~/.claude/settings.json."
            case .plugins: "No plugins in enabledPlugins."
            case .rules: "No cc-safety-net rulebooks are listed in rule.json."
            }
        }
    }

    @AppStorage("claudeSection") private var section: Section = .hooks
    @State private var adding: SettingDraft?
    @State private var query = ""

    var body: some View {
        VStack(spacing: 0) {
            ClaudeHeader(model: model, section: $section)
                .padding([.horizontal, .top])
                .padding(.bottom, 8)
            Divider()
            HSplitView {
                SettingsKindList(model: model, provider: section.provider, kind: section.kind, emptyText: section.emptyText,
                                 query: query)
                    .frame(minWidth: 420, idealWidth: 560, maxWidth: .infinity)
                SettingDetail(model: model)
                    .frame(minWidth: 280, idealWidth: 340, maxWidth: 520, maxHeight: .infinity)
            }
        }
        .navigationTitle("Claude")
        .searchable(text: $query, placement: .toolbar, prompt: "Filter \(section.title.lowercased())")
        .toolbar {
            ToolbarItem {
                Button {
                    if let kind = addableKind { adding = SettingDraft(provider: section.provider, kind: kind) }
                } label: { Label("Add", systemImage: "plus") }
                    .disabled(addableKind == nil)
                    .help(addableKind == nil ? "\(section.title) are not added here" : "Add to \(section.title)")
            }
            ToolbarItem {
                Button { Task { await model.load() } } label: { Label("Reload", systemImage: "arrow.clockwise") }
                    .help("Read the settings files again")
            }
        }
        .sheet(item: Binding(get: { adding.map(DraftBox.init) }, set: { adding = $0?.draft })) { box in
            SettingAddForm(model: model, draft: box.draft)
        }
        .onChange(of: section) { model.selection = nil }
        .task { await model.follow() }
    }

    private var addableKind: SettingKind? {
        guard let kind = model.listing?.provider(section.provider)?.kind(section.kind), kind.addable else { return nil }
        return kind
    }
}

/// Makes a draft presentable as a sheet item.
private struct DraftBox: Identifiable {
    let draft: SettingDraft
    var id: String { draft.provider + "/" + draft.kind.name }
}

private struct ClaudeHeader: View {
    let model: SettingsModel
    @Binding var section: ClaudeView.Section

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Picker("Section", selection: $section) {
                ForEach(ClaudeView.Section.allCases) { s in
                    Text(model.countLabel(s.title, provider: s.provider, kind: s.kind)).tag(s)
                }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            if let provider = model.listing?.provider(section.provider) {
                HStack(spacing: 6) {
                    Text(verbatim: displaySafe(provider.files.first ?? "")).textStyle(.codeCaption)
                        .lineLimit(1).truncationMode(.middle)
                    Spacer()
                    Label("This Mac only", systemImage: "desktopcomputer").textStyle(.caption)
                        .help("Choices are kept in this machine's orch.db and are not shared with other machines.")
                }
                if !provider.error.isEmpty {
                    Label { Text(verbatim: displaySafe(provider.error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                        .foregroundStyle(.red)
                        .font(.callout)
                        .textSelection(.enabled)
                        .help("Nothing is written until the file can be read again")
                }
            }
            if let error = model.lastError {
                Label { Text(verbatim: displaySafe(error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                    .foregroundStyle(.red)
                    .font(.callout)
            } else if let message = model.message {
                Text(verbatim: message).textStyle(.supporting).lineLimit(2)
            }
        }
    }
}
