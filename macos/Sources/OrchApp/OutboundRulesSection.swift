import OrchKit
import SwiftUI

/// Settings → Spaces → Outbound rules (SP-075): the outbound terms check's
/// baseline rules with a switch each, the stricter rules the operator added,
/// and a form to add one. A rule's value is sent to orchd and never shown
/// again (INV-07): rules are named by id.
struct OutboundRulesSection: View {
    let model: OutboundRulesModel
    /// Whether the outbound terms layer is off now, from the switches;
    /// nil until they are read.
    var termsLayerOff: Bool?
    @State private var kind = "fail_closed"
    @State private var value = ""

    var body: some View {
        Section {
            if let state = model.state {
                if termsLayerOff ?? state.termsOff {
                    Label("The outbound terms layer is off, so no rule applies. Switch it on under Enforcement.",
                          systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                        .textStyle(.supporting)
                }
                if !state.problem.isEmpty {
                    Label { Text(verbatim: displaySafe(state.problem)) } icon: {
                        Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                    }
                    .textStyle(.supporting)
                }
                ForEach(state.baselineRules) { rule in OutboundRuleRow(rule: rule, model: model) }
                ForEach(state.addedRules) { rule in OutboundRuleRow(rule: rule, model: model) }
                addForm(kinds: state.kinds)
                if let changed = changedLine(state) {
                    Text(verbatim: changed).textStyle(.caption)
                }
            } else if let error = model.lastError {
                Label { Text(verbatim: error) } icon: { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                    .textStyle(.supporting)
            } else {
                ProgressView().controlSize(.small)
            }
        } header: {
            Text("Outbound rules")
        } footer: {
            Text("How strictly the outbound terms check reads commands. Switching a baseline rule off relaxes it; a rule you add can only make it stricter. What a rule you add checks is never shown again: note it yourself. To turn the whole check off, switch the outbound terms layer above, or run `orch spaces enforce terms off`. Only you can change these, here or with `orch spaces terms rules`; every change is logged.")
                .textStyle(.caption)
        }
        .confirmationDialog(model.confirmation?.title ?? "Relax the outbound check?",
                            isPresented: Binding(get: { model.confirmation != nil }, set: { if !$0 { model.cancel() } }),
                            titleVisibility: .visible, presenting: model.confirmation) { _ in
            Button("Switch off", role: .destructive) { Task { await model.confirm() } }
            Button("Cancel", role: .cancel) { model.cancel() }
        } message: { c in
            Text(verbatim: displaySafe(c.message))
        }
    }

    private func addForm(kinds: [String]) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Add a stricter rule").textStyle(.rowTitle)
            HStack(spacing: 8) {
                Picker("Kind", selection: $kind) {
                    ForEach(kinds, id: \.self) { Text(verbatim: Self.kindTitle($0)).tag($0) }
                }
                .labelsHidden()
                .fixedSize()
                SecureField("What the rule checks", text: $value, prompt: Text(Self.placeholder(kind)))
                    .labelsHidden()
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: .infinity)
                Button("Add") {
                    Task {
                        if await model.add(kind: kind, value: value) { value = "" }
                    }
                }
                .disabled(value.trimmingCharacters(in: .whitespaces).isEmpty || model.pending.contains(""))
            }
            if let added = model.lastAdded {
                Text(verbatim: "Added rule \(displaySafe(added)).").textStyle(.caption)
            }
            if let error = model.ruleErrors[""] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private func changedLine(_ state: OutboundRulesState) -> String? {
        guard state.revision > 0 else { return nil }
        var parts = ["Rules revision \(state.revision)"]
        if let at = state.setAt { parts.append("changed " + at.formatted(date: .abbreviated, time: .shortened)) }
        if !state.setBy.isEmpty { parts.append("by " + displaySafe(state.setBy)) }
        return parts.joined(separator: " · ")
    }

    static func kindTitle(_ kind: String) -> String {
        switch kind {
        case "command": "Command"
        case "destination": "Destination"
        case "fail_closed": "Fail-closed pattern"
        default: displaySafe(kind)
        }
    }

    static func placeholder(_ kind: String) -> String {
        switch kind {
        case "command": "Program name, e.g. glab"
        case "destination": "owner or owner/repo"
        default: "Text, or re:<regex>"
        }
    }
}

/// One rule: its id, what it does, and a switch.
struct OutboundRuleRow: View {
    let rule: OutboundRule
    let model: OutboundRulesModel

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                Text(verbatim: displaySafe(rule.id)).font(.body.monospaced())
                if !rule.baseline {
                    Text(verbatim: OutboundRulesSection.kindTitle(rule.kind)).textStyle(.caption)
                }
                Spacer(minLength: 8)
                if model.pending.contains(rule.id) {
                    ProgressView().controlSize(.small).accessibilityLabel("Changing")
                }
                if !rule.baseline {
                    Button(role: .destructive) { Task { await model.remove(rule) } } label: {
                        Image(systemName: "trash")
                    }
                    .buttonStyle(.borderless)
                    .accessibilityLabel("Remove rule \(displaySafe(rule.id))")
                }
                Toggle("", isOn: Binding(get: { rule.on }, set: { on in Task { await model.set(rule, on: on) } }))
                    .labelsHidden()
                    .toggleStyle(.switch)
                    .accessibilityLabel("Rule \(displaySafe(rule.id))")
            }
            Text(verbatim: displaySafe(rule.summary) + (rule.loosens ? " (switched off, commands are read strictly)" : ""))
                .textStyle(.caption)
                .fixedSize(horizontal: false, vertical: true)
            if let error = model.ruleErrors[rule.id] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.vertical, 2)
    }
}
