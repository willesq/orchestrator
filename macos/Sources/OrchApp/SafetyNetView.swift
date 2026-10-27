import OrchKit
import SwiftUI

/// The Safety Net tab: whether cc-safety-net is installed in Claude Code at
/// the version orch pins, the rulebooks it enforces, and one-click actions.
/// Rulebook text and command output come from files and commands, so every
/// such string goes through displaySafe.
struct SafetyNetView: View {
    let model: SafetyNetModel

    var body: some View {
        VStack(spacing: 0) {
            SafetyNetHeader(model: model)
                .padding()
            Divider()
            RulebookList(state: model.state)
        }
        .navigationTitle("Safety Net")
        .toolbar {
            ToolbarItem {
                Button { Task { await model.load() } } label: { Label("Reload", systemImage: "arrow.clockwise") }
                    .disabled(model.busy)
                    .help("Read the plugin and rule files again")
            }
        }
        .task { await model.load() }
    }
}

private struct SafetyNetHeader: View {
    let model: SafetyNetModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let state = model.state {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text("cc-safety-net").font(.title3.weight(.semibold))
                    Badge(label: state.badge.label, tone: state.badge.tone)
                    Spacer()
                }
                Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                    row("Installed", state.installedVersion.isEmpty ? "not by orch" : "v\(state.installedVersion)")
                    row("Pinned", "v\(state.pinnedVersion) (\(String(state.pinnedSHA.prefix(12))), released \(state.pinnedReleasedOn))")
                    ForEach(state.legacy) { copy in
                        row("Also installed", "\(copy.pluginID) v\(copy.version) (\(copy.scope))")
                    }
                    row("Rules", state.rules.ok ? "orch-guards active" : state.rules.problems.joined(separator: "; "))
                    ForEach(Array(state.errors.enumerated()), id: \.offset) { _, e in row("Unreadable", e) }
                }
                .font(.callout)
                actions(state)
                progress(state)
            } else if model.lastError == nil {
                ProgressView().controlSize(.small)
            }
            if let error = model.lastError {
                Label { Text(verbatim: displaySafe(error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                    .foregroundStyle(.red)
                    .font(.callout)
                    .textSelection(.enabled)
            }
            if let message = model.message {
                Text(verbatim: message).font(.callout).foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder
    private func actions(_ state: SafetyNetState) -> some View {
        HStack {
            Button("Install") { Task { await model.install() } }
                .disabled(model.busy || !state.canInstall)
                .help("Install the pinned cc-safety-net into Claude Code, replacing any other copy")
            Button("Upgrade") { Task { await model.upgrade() } }
                .disabled(model.busy || !state.canUpgrade)
                .help("Move cc-safety-net to the version this orch pins")
            Button("Repair rules") { Task { await model.repairRules() } }
                .disabled(model.busy || !state.canRepairRules)
                .help("Rewrite the orch-guards rulebook and enable it, keeping your own rulebooks")
            if model.busy || state.action.running {
                ProgressView().controlSize(.small).padding(.leading, 4)
            }
        }
        .controlSize(.small)
    }

    @ViewBuilder
    private func progress(_ state: SafetyNetState) -> some View {
        let a = state.action
        if a.hasRun {
            VStack(alignment: .leading, spacing: 4) {
                if a.running {
                    Text(verbatim: displaySafe(a.step)).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                } else if let at = a.finishedAt {
                    HStack(spacing: 4) {
                        Image(systemName: a.ok ? "checkmark.circle" : "xmark.octagon")
                            .foregroundStyle(a.ok ? .green : .red)
                        Text(a.ok ? "Last action finished" : "Last action failed")
                        Text(at, format: .relative(presentation: .named))
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
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
            Text(label).foregroundStyle(.secondary)
            Text(verbatim: displaySafe(value)).textSelection(.enabled).lineLimit(3)
        }
    }
}

private struct Badge: View {
    let label: String
    let tone: SafetyNetState.Tone

    var body: some View {
        Text(label)
            .font(.caption.weight(.semibold))
            .padding(.horizontal, 8)
            .padding(.vertical, 2)
            .background(color.opacity(0.18), in: Capsule())
            .foregroundStyle(color)
    }

    private var color: Color {
        switch tone {
        case .good: .green
        case .warning: .orange
        case .bad: .red
        case .neutral: .secondary
        }
    }
}

private struct RulebookList: View {
    let state: SafetyNetState?

    var body: some View {
        let rulebooks = state?.rules.rulebooks ?? []
        List {
            ForEach(rulebooks) { rb in
                Section {
                    if !rb.error.isEmpty {
                        Label { Text(verbatim: displaySafe(rb.error)) } icon: { Image(systemName: "exclamationmark.triangle") }
                            .foregroundStyle(.red)
                            .help("cc-safety-net skips this rulebook until it is fixed")
                    }
                    ForEach(rb.rules) { rule in RuleRow(rule: rule) }
                } header: {
                    HStack(spacing: 6) {
                        Text(verbatim: displaySafe(rb.name)).font(.headline)
                        Text(rb.isOrch ? "orch" : "yours")
                            .font(.caption2)
                            .padding(.horizontal, 5)
                            .background(.quaternary, in: Capsule())
                            .help(rb.isOrch ? "Installed by orch and overwritten on upgrade" : "Your rulebook; orch never changes it")
                        if !rb.version.isEmpty { Text(verbatim: "v" + displaySafe(rb.version)).foregroundStyle(.secondary) }
                        if rb.remote { Text(verbatim: displaySafe(rb.source)).foregroundStyle(.secondary).lineLimit(1) }
                        Spacer()
                        Text("\(rb.rules.count) rule\(rb.rules.count == 1 ? "" : "s")").foregroundStyle(.secondary)
                    }
                }
            }
        }
        .overlay {
            if let state, rulebooks.isEmpty {
                Text(state.rules.configExists
                     ? "rule.json lists no rulebooks."
                     : "No rule.json yet. Repair rules creates it with the orch-guards rulebook.")
                    .multilineTextAlignment(.center)
                    .foregroundStyle(.secondary)
                    .padding()
            }
        }
    }
}

private struct RuleRow: View {
    let rule: SafetyNetRule

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                Text(verbatim: displaySafe(rule.name)).font(.callout.weight(.medium))
                    .strikethrough(rule.off)
                if rule.off { Text("off").font(.caption2).foregroundStyle(.secondary) }
                Spacer()
                Text(verbatim: displaySafe(rule.commandLine)).font(.caption.monospaced()).foregroundStyle(.secondary)
            }
            if !rule.blockArgs.isEmpty {
                Text(verbatim: "blocks " + displaySafe(rule.blockArgs.joined(separator: " ")))
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
            Text(verbatim: displaySafe(rule.reason)).font(.caption).foregroundStyle(.secondary).lineLimit(3)
        }
        .padding(.vertical, 2)
        .opacity(rule.off ? 0.6 : 1)
    }
}
