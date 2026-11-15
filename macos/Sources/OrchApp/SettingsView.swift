import OrchKit
import SwiftUI

/// The one place to configure orch: its settings, docs and GitHub opt-ins,
/// Claude Code's settings, the safety net and this app's own preferences.
struct SettingsView: View {
    let config: ConfigModel
    let docsOwners: DocsOwnersModel
    let github: GitHubModel
    let claude: SettingsModel
    let safetyNet: SafetyNetModel
    let loginItem: LoginItem
    let enforcement: EnforcementModel
    let placement: PlacementValidator
    @Bindable var navigator: Navigator

    @State private var confirmingRestart = false

    /// What the sidebar shows for the pane: the most pressing thing inside it.
    static func status(config: ConfigModel, safetyNet: SafetyNetModel, claude: SettingsModel) -> SidebarStatus {
        if config.state?.startupError.isEmpty == false { return .dot(.bad, "orchd could not start its services") }
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
                startupBanner
                restartBanner
                enforcementBanner
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
        case .overview:
            return config.state?.startupError.isEmpty == false ? .bad : nil
        case .spaces:
            return enforcement.relaxed.isEmpty ? nil : .warning
        default:
            let waiting = section.configSections.flatMap(config.fields(section:)).contains(where: \.pendingRestart)
            return waiting ? .warning : nil
        }
    }

    @ViewBuilder
    private var detail: some View {
        switch navigator.settingsSection {
        case .overview:
            SettingsOverview(config: config) { navigator.settingsSection = $0 }
                .navigationTitle("Overview")
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
        case .spaces:
            // The switch first; the policy's settings only while spaces are
            // on (an orchd without the switch always enforces).
            if config.fields(section: "Spaces").first(where: { $0.key == "spaces.enabled" })?.boolValue ?? true {
                VStack(spacing: 0) {
                    ConfigForm(config: config, sections: navigator.settingsSection.configSections)
                        .frame(maxHeight: 140)
                    Divider()
                    SpacesSettingsView(enforcement: enforcement, validator: placement)
                }
                .navigationTitle("Spaces")
            } else {
                ConfigForm(config: config, sections: navigator.settingsSection.configSections)
                    .navigationTitle("Spaces")
            }
        case .claude:
            ClaudeView(model: claude)
        case .safetyNet:
            SafetyNetView(model: safetyNet, settings: claude)
        case .app:
            Form {
                Section("This app") {
                    AppearancePicker()
                    DockIconToggle()
                    LoginItemRow(item: loginItem)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("App")
        }
    }

    /// orchd runs only to serve settings: one of them stopped its services.
    @ViewBuilder
    private var startupBanner: some View {
        if let error = config.state?.startupError, !error.isEmpty {
            HStack(spacing: 8) {
                Image(systemName: "exclamationmark.octagon.fill").foregroundStyle(.red)
                VStack(alignment: .leading, spacing: 1) {
                    Text("orchd could not start its services. Fix the setting below, then restart orchd.")
                    Text(verbatim: displaySafe(error)).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
                Spacer(minLength: 8)
                Button("Restart orchd…") { confirmingRestart = true }.disabled(config.isRestarting)
            }
            .padding(10)
            .background(.bar)
            Divider()
        }
    }

    /// While any enforcement layer is off, on every section.
    @ViewBuilder
    private var enforcementBanner: some View {
        if !enforcement.relaxed.isEmpty {
            EnforcementBanner(model: enforcement, show: navigator.settingsSection == .spaces ? nil : { navigator.settingsSection = .spaces })
                .padding(8)
            Divider()
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
/// half a value may not validate.
private struct ConfigFieldRow: View {
    let field: ConfigField
    let config: ConfigModel

    @State private var draft = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            control
                .disabled(config.pending.contains(field.key))
            if !field.help.isEmpty {
                Text(verbatim: field.help).textStyle(.caption)
            }
            if let error = config.fieldErrors[field.key] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
        }
        .contextMenu {
            if !field.isDefault {
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

/// Settings at a glance: where they live, what waits for a restart, and
/// leftovers worth tidying, offered quietly rather than as warnings.
private struct SettingsOverview: View {
    let config: ConfigModel
    let show: (SettingsSection) -> Void

    var body: some View {
        Form {
            Section {
                LabeledContent("Changed from default") {
                    Text(verbatim: "\(changed.count)")
                }
                ForEach(SettingsSection.allCases.filter { !$0.configSections.isEmpty }) { section in
                    let count = changed.filter { section.configSections.contains($0.section) }.count
                    if count > 0 {
                        Button { show(section) } label: {
                            LabeledContent(section.title) { Text(verbatim: "\(count) changed") }
                        }
                        .buttonStyle(.plain)
                    }
                }
                if config.pendingRestartCount > 0 {
                    LabeledContent("Waiting for a restart") { Text(verbatim: "\(config.pendingRestartCount)") }
                }
            } header: {
                Text("Settings")
            } footer: {
                Text("orch keeps its settings in its database; change them here.")
            }
            if let legacy = config.state?.legacyFile, !legacy.isEmpty {
                Section("Tidy up") {
                    HStack(alignment: .firstTextBaseline) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text("Legacy config.yaml found")
                            Text("orch no longer reads it; its settings were imported. Remove keeps a copy in orch's backups.")
                                .textStyle(.caption)
                            Text(verbatim: displaySafe(legacy)).textStyle(.codeCaption).textSelection(.enabled)
                            if let error = config.legacyError {
                                Text(verbatim: error).font(.caption).foregroundStyle(.red)
                            }
                        }
                        Spacer(minLength: 8)
                        if config.isRemovingLegacy {
                            ProgressView().controlSize(.small).accessibilityLabel("Removing config.yaml")
                        } else {
                            Button("Remove") { Task { await config.removeLegacyConfig() } }
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }

    private var changed: [ConfigField] { config.state?.fields.filter { !$0.isDefault } ?? [] }
}
