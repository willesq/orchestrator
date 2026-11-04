import OrchKit
import SwiftUI

/// The one place to configure orch: config.yaml, docs and GitHub opt-ins,
/// Claude Code's settings, the safety net and this app's own preferences.
struct SettingsView: View {
    let config: ConfigModel
    let docsOwners: DocsOwnersModel
    let github: GitHubModel
    let claude: SettingsModel
    let safetyNet: SafetyNetModel
    let loginItem: LoginItem
    @Bindable var navigator: Navigator

    @State private var confirmingRestart = false

    /// What the sidebar shows for the pane: the most pressing thing inside it.
    static func status(config: ConfigModel, safetyNet: SafetyNetModel, claude: SettingsModel) -> SidebarStatus {
        if config.state?.restartRequired == true { return .dot(.warning, "Waiting for orchd to restart") }
        if let badge = safetyNet.state?.badge, badge.tone != .good {
            return .dot(SafetyNetView.tone(badge.tone), "Safety Net: \(badge.label)")
        }
        let edited = claude.changedOutside
        return edited > 0 ? .dot(.warning, "\(edited) edited outside orch") : .none
    }

    var body: some View {
        HStack(spacing: 0) {
            List(SettingsSection.allCases, selection: Binding(get: { navigator.settingsSection },
                                                              set: { navigator.settingsSection = $0 ?? .tasks })) { section in
                HStack {
                    Label(section.title, systemImage: section.symbol)
                    Spacer(minLength: 4)
                    if let tone = tone(section) { StatusDot(tone: tone, label: "Needs attention") }
                }
                .tag(section)
            }
            .frame(width: 160)
            Divider()
            VStack(spacing: 0) {
                restartBanner
                detail.frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task { await config.load() }
        .confirmationDialog(confirmationTitle, isPresented: confirmationShown, titleVisibility: .visible,
                            presenting: config.confirmation) { _ in
            Button("Change Setting", role: .destructive) { Task { await config.confirmChange() } }
            Button("Cancel", role: .cancel) { config.cancelChange() }
        } message: { change in
            Text(verbatim: confirmationMessage(change))
        }
        .confirmationDialog("Restart orchd?", isPresented: $confirmingRestart, titleVisibility: .visible) {
            Button("Restart orchd") { Task { await config.restart() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Running tasks are interrupted and pick up again once it is back.")
        }
    }

    private func tone(_ section: SettingsSection) -> Tone? {
        switch section {
        case .safetyNet:
            guard let badge = safetyNet.state?.badge, badge.tone != .good else { return nil }
            return SafetyNetView.tone(badge.tone)
        case .claude:
            return claude.changedOutside > 0 ? .warning : nil
        default:
            let waiting = section.configSections.flatMap(config.fields(section:)).contains(where: \.pendingRestart)
            return waiting ? .warning : nil
        }
    }

    @ViewBuilder
    private var detail: some View {
        switch navigator.settingsSection {
        case .tasks, .memory:
            ConfigForm(config: config, sections: navigator.settingsSection.configSections)
                .navigationTitle(navigator.settingsSection.title)
        case .docs:
            ConfigForm(config: config, sections: navigator.settingsSection.configSections) {
                DocsOwnersSection(model: docsOwners)
            }
            .navigationTitle("Docs")
            .task { await docsOwners.load() }
        case .github:
            VStack(spacing: 0) {
                ScopesColumn(model: github)
                Divider()
                ConfigForm(config: config, sections: navigator.settingsSection.configSections)
                    .frame(maxHeight: 180)
            }
            .navigationTitle("GitHub")
            .onAppear { github.start() }
            .onDisappear { github.stop() }
        case .claude:
            ClaudeView(model: claude)
        case .safetyNet:
            SafetyNetView(model: safetyNet, settings: claude)
        case .app:
            Form {
                Section("This app") {
                    AppearancePicker()
                    LoginItemRow(item: loginItem)
                }
                if let path = config.state?.path {
                    Section("Configuration file") {
                        Text(verbatim: displaySafe(path)).textStyle(.supporting).textSelection(.enabled)
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("App")
        }
    }

    @ViewBuilder
    private var restartBanner: some View {
        if config.state?.restartRequired == true || config.isRestarting || config.restartError != nil {
            HStack(spacing: 8) {
                Image(systemName: "arrow.clockwise.circle.fill").foregroundStyle(.orange)
                VStack(alignment: .leading, spacing: 1) {
                    Text(verbatim: bannerText)
                    if let error = config.restartError {
                        Text(verbatim: error).font(.caption).foregroundStyle(.red)
                    }
                }
                Spacer(minLength: 8)
                if config.isRestarting {
                    ProgressView().controlSize(.small).accessibilityLabel("Restarting orchd")
                } else {
                    Button("Restart orchd…") { confirmingRestart = true }
                }
            }
            .padding(10)
            .background(.bar)
            Divider()
        }
    }

    private var bannerText: String {
        if config.isRestarting { return "Restarting orchd…" }
        let n = config.pendingRestartCount
        return n == 1 ? "1 change takes effect when orchd restarts." : "\(n) changes take effect when orchd restarts."
    }

    private var confirmationShown: Binding<Bool> {
        Binding(get: { config.confirmation != nil }, set: { if !$0 { config.cancelChange() } })
    }

    private var confirmationTitle: String {
        "Change \(config.confirmation?.field.label ?? "this setting")?"
    }

    private func confirmationMessage(_ change: ConfigChange) -> String {
        let help = change.field.help.isEmpty ? "" : change.field.help + "\n\n"
        return help + "This setting changes what agents may do or where orch keeps its data.\n\nNew value: " + displaySafe(ConfigJSON.pretty(change.json))
    }
}

/// config.yaml's settings for some sections, each row built from how orchd
/// describes it. Anything above them goes in `header`.
private struct ConfigForm<Header: View>: View {
    let config: ConfigModel
    let sections: [String]
    @ViewBuilder var header: Header

    init(config: ConfigModel, sections: [String], @ViewBuilder header: () -> Header = { EmptyView() }) {
        self.config = config
        self.sections = sections
        self.header = header()
    }

    var body: some View {
        Form {
            header
            if let error = config.lastError {
                Section {
                    Text(verbatim: "Could not read the settings: \(error)").foregroundStyle(.red)
                    Button("Try Again") { Task { await config.load() } }
                }
            } else if config.state == nil {
                ProgressView().frame(maxWidth: .infinity)
            }
            ForEach(sections, id: \.self) { section in
                let fields = config.fields(section: section)
                if !fields.isEmpty {
                    Section(section) {
                        ForEach(fields) { ConfigFieldRow(field: $0, config: config) }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }
}

/// One setting: a control for its type, what it means, and anything wrong with
/// the last change. Text is written when submitted, not per keystroke, since
/// each write is a whole-file rewrite and half a value may not validate.
private struct ConfigFieldRow: View {
    let field: ConfigField
    let config: ConfigModel

    @State private var draft = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            control
                .disabled(field.envOverride || config.pending.contains(field.key))
            if !field.help.isEmpty {
                Text(verbatim: field.help).textStyle(.caption)
            }
            if field.envOverride {
                Text("An environment variable sets this, so it cannot be changed here.")
                    .font(.caption).foregroundStyle(.orange)
            }
            if let error = config.fieldErrors[field.key] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
        }
        .contextMenu {
            if !field.isDefault && !field.envOverride {
                Button("Reset to Default") { Task { await config.resetToDefault(field) } }
            }
        }
        .onAppear { draft = editableText }
        .onChange(of: field.value) { draft = editableText }
        // A sensitive change the user backed out of leaves the old value showing.
        .onChange(of: config.confirmation == nil) { if config.confirmation == nil { draft = editableText } }
    }

    private var editableText: String {
        field.type == .list || field.type == .map ? field.multilineText : field.text
    }

    private var title: some View {
        HStack(spacing: 6) {
            Text(verbatim: field.label)
            if field.sensitive {
                Image(systemName: "exclamationmark.shield")
                    .foregroundStyle(Color.secondaryText)
                    .help("Asks before changing: this affects what agents may do or where orch keeps its data")
                    .accessibilityLabel("Sensitive")
            }
            if field.pendingRestart {
                Text("after restart").textStyle(.caption)
            }
        }
    }

    @ViewBuilder
    private var control: some View {
        switch field.type {
        case .bool:
            Toggle(isOn: Binding(get: { field.boolValue },
                                 set: { on in Task { await config.change(field, json: on ? "true" : "false") } })) { title }
        case .int:
            LabeledContent {
                HStack(spacing: 6) {
                    TextField("", text: $draft)
                        .labelsHidden()
                        .multilineTextAlignment(.trailing)
                        .frame(width: 70)
                        .onSubmit(commit)
                    if let range = field.range {
                        Stepper("", value: Binding(get: { field.intValue },
                                                   set: { n in Task { await config.changeText(field, text: String(n)) } }),
                                in: Int(range.lowerBound)...Int(range.upperBound))
                            .labelsHidden()
                    }
                }
            } label: { title }
        case .enum:
            Picker(selection: Binding(get: { field.text }, set: { v in Task { await config.changeText(field, text: v) } })) {
                ForEach(field.options, id: \.self) { Text(verbatim: $0).tag($0) }
            } label: { title }
        case .list, .map:
            VStack(alignment: .leading, spacing: 4) {
                title
                TextEditor(text: $draft)
                    .font(.system(.body, design: .monospaced))
                    .frame(minHeight: 70, maxHeight: 160)
                    .overlay(RoundedRectangle(cornerRadius: 4).stroke(Color.surfaceStroke))
                HStack {
                    Text(field.type == .list ? "One per line." : "JSON.").textStyle(.caption)
                    Spacer()
                    Button("Revert") { draft = editableText }.disabled(draft == editableText)
                    Button("Save", action: commit).disabled(draft == editableText)
                }
            }
        default:
            LabeledContent {
                TextField("", text: $draft, prompt: field.type == .path ? Text("Default") : nil)
                    .labelsHidden()
                    .multilineTextAlignment(.trailing)
                    .onSubmit(commit)
            } label: { title }
        }
    }

    private func commit() {
        Task { await config.changeText(field, text: draft) }
    }
}
