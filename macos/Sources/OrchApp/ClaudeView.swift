import OrchKit
import SwiftUI

/// The Claude tab: Claude Code's hooks, permission rules and plugins from
/// ~/.claude/settings.json, cc-safety-net's rules, and orch's instruction
/// modules (the pieces of CLAUDE.md, GEMINI.md and Codex's AGENTS.md that
/// orch manages), each with a toggle.
/// A change is written to the file at once and kept in this machine's
/// orch.db; edits made to the files elsewhere show up within seconds.
struct ClaudeView: View {
    let model: SettingsModel

    /// One section of the tab: a provider's kind.
    enum Section: String, CaseIterable, Identifiable {
        case hooks, permissions, plugins, rules, instructions

        var id: String { rawValue }

        var title: String {
            switch self {
            case .hooks: "Hooks"
            case .permissions: "Permissions"
            case .plugins: "Plugins"
            case .rules: "Safety Net rules"
            case .instructions: "Instructions"
            }
        }

        var provider: String {
            switch self {
            case .rules: "cc-safety-net"
            case .instructions: "orch-instructions"
            default: "claude-code"
            }
        }

        var kind: String {
            switch self {
            case .hooks: "hook"
            case .permissions: "permission"
            case .plugins: "plugin"
            case .rules: "rule"
            case .instructions: "module"
            }
        }

        /// How wide the list may be. Instruction rows are a title and a
        /// sentence, so they cede room to the module text beside them.
        var listWidth: (min: CGFloat, ideal: CGFloat) {
            self == .instructions ? (340, 420) : (420, 560)
        }

        /// How wide the detail pane may be. A module's rendered text needs
        /// room to read; other items show a command and a few fields.
        var detailWidth: (min: CGFloat, ideal: CGFloat, max: CGFloat) {
            self == .instructions ? (400, 560, 960) : (280, 340, 520)
        }

        var emptyText: String {
            switch self {
            case .hooks: "No hooks in ~/.claude/settings.json."
            case .permissions: "No permission rules in ~/.claude/settings.json."
            case .plugins: "No plugins in enabledPlugins."
            case .rules: "No cc-safety-net rulebooks are listed in rule.json."
            case .instructions: "No instruction modules. Run orch install --apply to wire your agents."
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
                    .frame(minWidth: section.listWidth.min, idealWidth: section.listWidth.ideal, maxWidth: .infinity)
                SettingDetail(model: model)
                    .frame(minWidth: section.detailWidth.min, idealWidth: section.detailWidth.ideal,
                           maxWidth: section.detailWidth.max, maxHeight: .infinity)
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
