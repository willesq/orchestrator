import AppKit
import OrchKit
import SwiftUI

/// Settings → Spaces: the enforcement switches with their timers, and the
/// directory validator. Everything here is the operator's: orchd refuses a
/// switch from anything an agent runs, and says so under the layer.
struct SpacesSettingsView: View {
    let enforcement: EnforcementModel
    @Bindable var validator: PlacementValidator

    var body: some View {
        Form {
            Section {
                if let error = enforcement.lastError, enforcement.state == nil {
                    Label { Text(verbatim: error) } icon: { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                        .textStyle(.supporting)
                }
                if let state = enforcement.state {
                    if !state.error.isEmpty {
                        Label { Text(verbatim: displaySafe(state.error)) } icon: {
                            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                        }
                        .textStyle(.supporting)
                    }
                    ForEach(state.switches) { layer in
                        EnforcementRow(layer: layer, model: enforcement)
                    }
                } else if enforcement.lastError == nil {
                    ProgressView().controlSize(.small)
                }
            } header: {
                Text("Enforcement")
            } footer: {
                Text("Only you can switch a layer, here or with `orch spaces enforce`. Every change is logged with its time, and while a layer is off every session's start digest says so. A timed switch reverts by itself.")
                    .textStyle(.caption)
            }
            Section {
                PlacementCheck(validator: validator)
            } header: {
                Text("Check a folder")
            } footer: {
                Text("Shows every root holding the folder, shallowest first, what its git remote changes, and the space orchd places its content in.")
                    .textStyle(.caption)
            }
        }
        .formStyle(.grouped)
        .enforcementConfirmation(enforcement)
        .task { await enforcement.load() }
    }
}

/// One layer: its name, state and timer, and a menu to switch it.
struct EnforcementRow: View {
    let layer: EnforcementSwitch
    let model: EnforcementModel
    var compact = false

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                StatusDot(tone: layer.relaxed ? .warning : .good, label: layer.relaxed ? "Not enforcing" : "Enforcing")
                Text(verbatim: displaySafe(layer.name.prefix(1).uppercased() + layer.name.dropFirst()))
                    .textStyle(.rowTitle)
                Spacer(minLength: 8)
                if model.pending.contains(layer.layer) {
                    ProgressView().controlSize(.small).accessibilityLabel("Switching")
                }
                EnforcementMenu(layer: layer, model: model)
                    .fixedSize()
                    .controlSize(compact ? .small : .regular)
            }
            if let detail = detail {
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Text(verbatim: line(detail, at: context.date))
                        .textStyle(.caption)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            if !layer.problem.isEmpty {
                Text(verbatim: displaySafe(layer.problem)).font(.caption).foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let error = model.layerErrors[layer.layer] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !compact, let notes = model.notes[layer.layer], !notes.isEmpty {
                ForEach(notes, id: \.self) { Text(verbatim: $0).textStyle(.caption) }
            }
        }
        .padding(.vertical, compact ? 0 : 2)
    }

    /// What the caption line says, before the timer is filled in.
    private var detail: (since: Date?, setBy: String)? {
        guard layer.setAt != nil || layer.until != nil else { return nil }
        return (layer.setAt, layer.setBy)
    }

    private func line(_ detail: (since: Date?, setBy: String), at now: Date) -> String {
        var parts: [String] = []
        if let since = detail.since {
            parts.append("Since " + since.formatted(date: .abbreviated, time: .shortened))
        }
        if !compact, !detail.setBy.isEmpty { parts.append("by " + displaySafe(detail.setBy)) }
        if let countdown = layer.countdown(at: now) { parts.append(countdown) }
        return parts.joined(separator: " · ")
    }
}

/// The state picker: every state, each with how long it should hold.
private struct EnforcementMenu: View {
    let layer: EnforcementSwitch
    let model: EnforcementModel

    var body: some View {
        Menu {
            ForEach(layer.states, id: \.self) { state in
                Menu {
                    ForEach(EnforcementDuration.allCases) { duration in
                        Button(duration.label) { Task { await model.choose(layer, state: state, duration: duration) } }
                            .disabled(state == layer.state && duration == .untilChanged)
                    }
                } label: {
                    Text(verbatim: displaySafe(label(state)))
                }
            }
        } label: {
            Text(verbatim: displaySafe(layer.state))
        }
        .disabled(model.pending.contains(layer.layer))
        .accessibilityLabel(displaySafe("\(layer.name): \(layer.state)"))
    }

    private func label(_ state: String) -> String {
        var text = state
        if state == layer.defaultState { text += " (default)" }
        if state == layer.state { text += " ✓" }
        return text
    }
}

/// Shown while any layer is not enforcing: which, and for how long, with a
/// way to the switches.
struct EnforcementBanner: View {
    let model: EnforcementModel
    let show: (() -> Void)?

    var body: some View {
        let relaxed = model.relaxed
        if !relaxed.isEmpty {
            HStack(alignment: .top, spacing: 8) {
                Image(systemName: "exclamationmark.shield.fill").foregroundStyle(.orange)
                    .accessibilityHidden(true)
                VStack(alignment: .leading, spacing: 2) {
                    Text(relaxed.count == 1 ? "Enforcement is off for 1 layer" : "Enforcement is off for \(relaxed.count) layers")
                        .font(.callout.weight(.semibold))
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        Text(verbatim: summary(relaxed, at: context.date))
                            .textStyle(.caption)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                Spacer(minLength: 8)
                if let show { Button("Show Switches", action: show) }
            }
            .padding(10)
            .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
            .accessibilityElement(children: .combine)
        }
    }

    private func summary(_ layers: [EnforcementSwitch], at now: Date) -> String {
        layers.map { layer in
            var text = displaySafe(layer.name) + ": " + (layer.problem.isEmpty ? displaySafe(layer.state) : "not enforcing")
            if let left = layer.remaining(at: now) { text += " for " + EnforcementSwitch.clock(left) }
            return text
        }
        .joined(separator: " · ")
    }
}

/// The directory validator: a path field, Choose…, and orchd's answer.
private struct PlacementCheck: View {
    @Bindable var validator: PlacementValidator

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                TextField("Folder", text: $validator.path, prompt: Text(verbatim: "~/code/project"))
                    .textFieldStyle(.roundedBorder)
                    .font(TextStyle.code.font)
                    .labelsHidden()
                    .onSubmit { Task { await validator.check() } }
                Button("Choose…", action: choose)
                Button("Check") { Task { await validator.check() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(validator.isChecking)
            }
            if validator.isChecking { ProgressView().controlSize(.small) }
            if let error = validator.error {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
            if let answer = validator.explanation, let path = validator.checked {
                PlacementResult(path: path, answer: answer, chain: validator.chain)
            }
        }
    }

    private func choose() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.prompt = "Check"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await validator.check(picked: url.path) }
    }
}

/// orchd's answer: the roots from shallowest to deepest, the remote, the
/// effective space and why.
struct PlacementResult: View {
    let path: String
    let answer: PlacementExplanation
    let chain: [(root: PlacementRoot, decides: Bool)]

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(verbatim: displaySafe(path)).textStyle(.codeCaption).textSelection(.enabled)
            if chain.isEmpty {
                Text("No root holds it.").textStyle(.supporting)
            } else {
                ForEach(Array(chain.enumerated()), id: \.offset) { index, step in
                    HStack(spacing: 6) {
                        Image(systemName: index == 0 ? "folder" : "arrow.turn.down.right")
                            .foregroundStyle(Color.secondaryText).accessibilityHidden(true)
                        Text(verbatim: displaySafe(step.root.root)).textStyle(.code).lineLimit(1).truncationMode(.middle)
                        Chip(text: displaySafe(step.root.space), tone: tone(kind: step.root.kind))
                        Text(verbatim: displaySafe(step.root.kind)).textStyle(.caption)
                        if step.decides { Chip(text: "deepest", tone: .accent) }
                    }
                    .padding(.leading, CGFloat(index) * 12)
                }
            }
            if let remote = answer.remote {
                HStack(spacing: 6) {
                    Image(systemName: "arrow.triangle.branch").foregroundStyle(Color.secondaryText).accessibilityHidden(true)
                    Text(verbatim: "Remote " + displaySafe(remote.label)).textStyle(.supporting)
                    Text(verbatim: displaySafe(remote.effect)).textStyle(.caption)
                    if answer.remoteAdjusted { Chip(text: "changes the result", tone: .warning) }
                }
            }
            Divider()
            HStack(spacing: 6) {
                Text("Space").textStyle(.supporting)
                Chip(text: displaySafe(answer.result), tone: answer.outcome == "placed" ? .good : .bad,
                     symbol: answer.outcome == "placed" ? "checkmark" : "exclamationmark.triangle")
                Text(verbatim: displaySafe(answer.rule)).textStyle(.codeCaption)
            }
            Text(verbatim: displaySafe(answer.reason)).textStyle(.supporting).fixedSize(horizontal: false, vertical: true)
            if !answer.sessionSpace.isEmpty {
                Text(verbatim: "A session started here writes to \(displaySafe(answer.sessionSpace)) and reads "
                    + (answer.sessionReads.isEmpty ? "public spaces only." : displaySafe(answer.sessionReads) + " and public spaces."))
                    .textStyle(.caption)
            }
        }
        .padding(.top, 4)
    }

    private func tone(kind: String) -> Tone {
        switch kind {
        case "public": .good
        case "isolated": .warning
        default: .accent
        }
    }
}

extension View {
    /// Asks before a switch stops a layer enforcing.
    func enforcementConfirmation(_ model: EnforcementModel) -> some View {
        confirmationDialog(model.confirmation?.title ?? "Change enforcement?",
                           isPresented: Binding(get: { model.confirmation != nil }, set: { if !$0 { model.cancel() } }),
                           titleVisibility: .visible, presenting: model.confirmation) { change in
            Button("Switch to \(displaySafe(change.state))", role: .destructive) { Task { await model.confirm() } }
            Button("Cancel", role: .cancel) { model.cancel() }
        } message: { change in
            Text(verbatim: change.message)
        }
    }
}
