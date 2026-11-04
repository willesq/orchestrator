import OrchKit
import SwiftUI

/// The Safety Net tab: whether cc-safety-net is installed in Claude Code at
/// the version orch pins, the rulebooks it enforces with a switch per rule,
/// and the one action that applies. Rulebook text and command output come
/// from files and commands, so every such string goes through displaySafe.
struct SafetyNetView: View {
    let model: SafetyNetModel
    /// The same settings the Claude tab shows: rule switches go through it,
    /// so both tabs agree on what is on.
    let settings: SettingsModel

    var body: some View {
        VStack(spacing: 0) {
            SafetyNetHeader(model: model)
                .padding(Metrics.pagePadding)
            Divider()
            RulebookList(state: model.state, settings: settings)
        }
        .navigationTitle("Safety Net")
        .toolbar {
            ToolbarItem {
                Button {
                    Task {
                        await model.load()
                        await settings.load()
                    }
                } label: { Label("Reload", systemImage: "arrow.clockwise") }
                    .disabled(model.busy)
                    .help("Read the plugin and rule files again")
            }
        }
        .task {
            async let a: Void = model.load()
            async let b: Void = settings.load()
            _ = await (a, b)
        }
        // A rule switched here or in the Claude tab changes rule.json; read
        // the plugin state again so the header and struck rules follow.
        .onChange(of: settings.listing?.revision) { Task { await model.load() } }
    }

    /// The shared tone for a Safety Net state.
    static func tone(_ tone: SafetyNetState.Tone) -> Tone {
        switch tone {
        case .good: .good
        case .warning: .warning
        case .bad: .bad
        case .neutral: .neutral
        }
    }
}

private struct SafetyNetHeader: View {
    let model: SafetyNetModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let state = model.state {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text("cc-safety-net").textStyle(.title)
                    Chip(text: state.badge.label, tone: SafetyNetView.tone(state.badge.tone),
                         symbol: state.badge.tone == .good ? "checkmark.shield.fill" : "exclamationmark.shield.fill")
                    Spacer()
                    action(state)
                }
                Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 4) {
                    row("Installed", state.installedVersion.isEmpty ? "not by orch" : "v\(state.installedVersion)")
                    row("Pinned", "v\(state.pinnedVersion) (\(String(state.pinnedSHA.prefix(12))), released \(state.pinnedReleasedOn))")
                    ForEach(state.legacy) { copy in
                        row("Also installed", "\(copy.pluginID) v\(copy.version) (\(copy.scope))")
                    }
                    row("Rules", state.rules.ok ? "orch-guards active" : state.rules.problems.joined(separator: "; "))
                    ForEach(Array(state.errors.enumerated()), id: \.offset) { _, e in row("Unreadable", e) }
                }
                .font(.callout)
                progress(state)
            } else if model.lastError == nil {
                ProgressView().controlSize(.small)
            }
            if let error = model.lastError {
                ErrorText(displaySafe(error), symbol: "exclamationmark.triangle", font: .callout)
            }
            if let message = model.message {
                Text(verbatim: message).textStyle(.supporting)
            }
        }
        .padding(Metrics.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .cardSurface()
    }

    /// Only the action that applies, as the primary button; otherwise a
    /// plain "Up to date" so nothing disabled sits there to be puzzled over.
    @ViewBuilder
    private func action(_ state: SafetyNetState) -> some View {
        if model.busy || state.action.running {
            HStack(spacing: 6) {
                ProgressView().controlSize(.small)
                Text("Working…").textStyle(.caption)
            }
        } else if let next = state.nextAction {
            Button(next.title) { Task { await model.perform(next) } }
                .buttonStyle(.borderedProminent)
                .help(next.help)
        } else {
            Label("Up to date", systemImage: "checkmark.circle.fill")
                .font(.callout)
                .foregroundStyle(.green)
        }
    }

    @ViewBuilder
    private func progress(_ state: SafetyNetState) -> some View {
        let a = state.action
        if a.hasRun {
            VStack(alignment: .leading, spacing: 4) {
                if a.running {
                    Text(verbatim: displaySafe(a.step)).textStyle(.caption).lineLimit(2)
                } else if let at = a.finishedAt {
                    HStack(spacing: 4) {
                        Image(systemName: a.ok ? "checkmark.circle" : "xmark.octagon")
                            .foregroundStyle(a.ok ? .green : .red)
                        Text(a.ok ? "Last action finished" : "Last action failed")
                        Text(at, format: .relative(presentation: .named))
                    }
                    .textStyle(.caption)
                }
                if !a.log.isEmpty {
                    DisclosureGroup("Output") {
                        ScrollView {
                            Text(verbatim: a.displayLog)
                                .font(.caption.monospaced())
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .frame(maxHeight: 160)
                    }
                    .font(.caption)
                }
            }
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(Color.secondaryText)
            Text(verbatim: displaySafe(value)).textSelection(.enabled).lineLimit(3)
        }
    }
}

private struct RulebookList: View {
    let state: SafetyNetState?
    let settings: SettingsModel

    var body: some View {
        let rulebooks = state?.rules.rulebooks ?? []
        let kind = settings.listing?.provider(SafetyNetRulebook.settingsProvider)?.kind("rule")
        List {
            ForEach(rulebooks) { rb in
                Section {
                    if !rb.error.isEmpty {
                        ErrorText(displaySafe(rb.error), symbol: "exclamationmark.triangle", font: .body)
                            .help("cc-safety-net skips this rulebook until it is fixed")
                    }
                    ForEach(rb.rules) { rule in
                        RuleRow(rule: rule, item: item(rb, rule), kind: kind, settings: settings)
                    }
                } header: {
                    HStack(spacing: 6) {
                        Text(verbatim: displaySafe(rb.name)).textStyle(.section)
                        Chip(text: rb.isOrch ? "orch" : "yours", tone: rb.isOrch ? .accent : .good,
                             help: rb.isOrch ? "Installed by orch and overwritten on upgrade" : "Your rulebook; orch never changes it")
                        if !rb.version.isEmpty { Text(verbatim: "v" + displaySafe(rb.version)).textStyle(.caption) }
                        if rb.remote { Text(verbatim: displaySafe(rb.source)).textStyle(.codeCaption).lineLimit(1) }
                        Spacer()
                        let on = rb.rules.filter { item(rb, $0)?.isOn ?? !$0.off }.count
                        Text(on == rb.rules.count ? "\(on) on" : "\(on) of \(rb.rules.count) on").textStyle(.caption)
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityAddTraits(.isHeader)
                }
            }
        }
        .overlay {
            if let state, rulebooks.isEmpty {
                EmptyState(symbol: "shield.slash", title: "No rulebooks",
                           message: state.rules.configExists
                               ? "rule.json lists no rulebooks."
                               : "There is no rule.json yet. Repair rules creates it with the orch-guards rulebook.")
            }
        }
    }

    private func item(_ rb: SafetyNetRulebook, _ rule: SafetyNetRule) -> SettingItem? {
        let ref = rb.settingRef(for: rule)
        return settings.listing?.items.first { $0.ref == ref }
    }
}

/// One rule as a sentence: what it blocks, and why underneath, with the
/// same switch the Claude tab shows.
private struct RuleRow: View {
    let rule: SafetyNetRule
    let item: SettingItem?
    let kind: SettingKind?
    let settings: SettingsModel

    var body: some View {
        let on = item?.isOn ?? !rule.off
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            if let item {
                SettingToggle(model: settings, item: item, kind: kind, label: displaySafe(rule.name))
                    .alignmentGuide(.firstTextBaseline) { $0[VerticalAlignment.center] + 4 }
            }
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(verbatim: displaySafe(rule.name)).font(.body.weight(.medium))
                    if !on { Chip(text: "off") }
                }
                (Text("Blocks ") + Text(verbatim: displaySafe(rule.blockedPhrase)).font(TextStyle.code.font))
                    .font(.callout)
                    .lineLimit(2)
                if !rule.reason.isEmpty {
                    Text(verbatim: displaySafe(rule.reason)).textStyle(.caption).lineLimit(3)
                }
                if let item, let error = settings.itemErrors[item.ref] {
                    ErrorText(displaySafe(error), symbol: "exclamationmark.triangle")
                }
            }
            Spacer(minLength: 4)
            if let item, settings.pending.contains(item.ref) { ProgressView().controlSize(.small) }
        }
        .padding(.vertical, 3)
        .opacity(on ? 1 : 0.7)
        .accessibilityElement(children: .contain)
    }
}
