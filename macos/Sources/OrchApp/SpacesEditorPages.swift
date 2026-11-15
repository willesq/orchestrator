import OrchKit
import SwiftUI

// MARK: Access

/// Who reads whom, as a matrix, and where each kind of content goes.
struct AccessEditor: View {
    let model: SpacesEditorModel
    /// A read that shares personal content with a job, waiting for the user.
    @State private var pending: ReadsCell?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
            Card("Reads") {
                let matrix = model.matrix
                if matrix.spaces.isEmpty {
                    Text("No spaces yet.").textStyle(.supporting)
                } else {
                    ScrollView(.horizontal) {
                        Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 6) {
                            GridRow {
                                Text("Reader ↓ reads →").textStyle(.caption)
                                ForEach(matrix.spaces) { Text(verbatim: displaySafe($0.name)).textStyle(.caption).lineLimit(1) }
                            }
                            ForEach(Array(zip(matrix.spaces, matrix.rows)), id: \.0.id) { reader, cells in
                                GridRow {
                                    HStack(spacing: 4) {
                                        Text(verbatim: displaySafe(reader.name)).textStyle(.rowTitle).lineLimit(1)
                                        Text(verbatim: displaySafe(reader.kind)).textStyle(.caption)
                                    }
                                    ForEach(cells) { cell in
                                        ReadsCellView(cell: cell) { on in
                                            if on, case .switchable(warning: .some) = cell.state {
                                                pending = cell
                                            } else {
                                                model.setRead(reader: cell.reader, target: cell.target, on: on)
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                    if let error = model.accessError {
                        Text(verbatim: error).font(.caption).foregroundStyle(.red)
                    }
                    Text("A space always reads itself and every public space. Locked cells are reads orchd refuses: a public space reads only public ones, and nothing reads a job's space but the job.")
                        .textStyle(.caption)
                }
            }
            Card("Where each kind of content goes") {
                ForEach(model.contentKinds) { row in
                    VStack(alignment: .leading, spacing: 2) {
                        Text(verbatim: row.kind).textStyle(.rowTitle)
                        Text(verbatim: row.goesTo).textStyle(.supporting).fixedSize(horizontal: false, vertical: true)
                        Text(verbatim: "Read by: " + row.readBy).textStyle(.caption)
                    }
                    .padding(.vertical, 2)
                }
                Text("These are orchd's fixed rules; the policy has no per-kind settings yet.").textStyle(.caption)
            }
        }
        .confirmationDialog("Share personal content with a job?", isPresented: Binding(get: { pending != nil }, set: { if !$0 { pending = nil } }),
                            titleVisibility: .visible, presenting: pending) { cell in
            Button("Let \(displaySafe(cell.reader)) Read \(displaySafe(cell.target))") {
                model.setRead(reader: cell.reader, target: cell.target, on: true)
            }
        } message: { cell in
            if case .switchable(let warning?) = cell.state { Text(verbatim: warning) }
        }
    }
}

/// One matrix cell: fixed, locked or a checkbox.
private struct ReadsCellView: View {
    let cell: ReadsCell
    let set: (Bool) -> Void

    var body: some View {
        switch cell.state {
        case .itself:
            Image(systemName: "minus").foregroundStyle(Color.secondaryText).help("A space reads itself")
        case .always:
            Image(systemName: "checkmark.circle").foregroundStyle(Color.secondaryText).help("Every space reads a public space")
        case .locked(let why):
            if cell.listed {
                Button { set(false) } label: { Image(systemName: "lock.trianglebadge.exclamationmark").foregroundStyle(.red) }
                    .buttonStyle(.borderless)
                    .help(why + " The draft lists it anyway; click to switch it off.")
            } else {
                Image(systemName: "lock.fill").foregroundStyle(Color.secondaryText).help(why)
                    .accessibilityLabel("Locked: " + why)
            }
        case .switchable(let warning):
            Toggle(isOn: Binding(get: { cell.listed }, set: { set($0) })) { EmptyView() }
                .toggleStyle(.checkbox)
                .labelsHidden()
                .help(warning ?? "Sessions of \(displaySafe(cell.reader)) read \(displaySafe(cell.target))")
                .overlay(alignment: .topTrailing) {
                    if warning != nil { Image(systemName: "exclamationmark.triangle.fill").font(.system(size: 7)).foregroundStyle(.orange).offset(x: 6, y: -3) }
                }
        }
    }
}

// MARK: Rules

/// The "where would this go?" tester and the rules the policy generates.
struct RulesEditor: View {
    let model: SpacesEditorModel

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
            Card("Where would this go?") {
                TesterField(tester: model.tester)
                Text("Asks orchd about the applied policy: a folder, a web address, owner/repo, or a chat tenant (slack:<id>, teams:<id>). Save and apply a draft to test it.")
                    .textStyle(.caption)
            }
            Card("Generated rules") {
                let rules = model.rules
                if rules.isEmpty { Text("The draft binds nothing yet.").textStyle(.supporting) }
                ForEach(rules) { rule in
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text(rule.kind.title).textStyle(.caption)
                            Text(verbatim: displaySafe(rule.value)).textStyle(.code).lineLimit(1).truncationMode(.middle)
                            Image(systemName: "arrow.right").foregroundStyle(Color.secondaryText).accessibilityHidden(true)
                            Chip(text: displaySafe(rule.space), tone: kindTone(rule.spaceKind))
                            Spacer(minLength: 6)
                            if let n = rule.matches {
                                Text(n == 1 ? "1 source" : "\(n) sources").textStyle(.caption)
                                    .help("Stored sources whose address this rule names")
                            }
                        }
                        Text(rule.kind.effect).textStyle(.caption)
                    }
                }
                Text("Roots are listed deepest first, as the deepest decides. Counts are of the sources orchd lists, up to \(SpacesEditorModel.sourceLimit).")
                    .textStyle(.caption)
            }
        }
    }
}

private struct TesterField: View {
    @Bindable var tester: PlacementTester

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                TextField("Target", text: $tester.input, prompt: Text(verbatim: "~/code/acme, https://…, acme/app, slack:T0123"))
                    .textFieldStyle(.roundedBorder).font(TextStyle.code.font).labelsHidden()
                    .onSubmit { Task { await tester.check() } }
                Button("Check") { Task { await tester.check() } }.disabled(tester.isChecking)
            }
            if tester.isChecking { ProgressView().controlSize(.small) }
            if let error = tester.error { Text(verbatim: error).font(.caption).foregroundStyle(.red) }
            if let answer = tester.explanation, let target = tester.checked {
                PlacementResult(path: target.target, answer: answer, chain: tester.chain)
            }
        }
    }
}

// MARK: Coverage

/// What each harness's hooks cover, the sessions orchd sees, and the
/// enforcement switches.
struct CoverageEditor: View {
    let model: SpacesEditorModel
    let enforcement: EnforcementModel

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
            Card("Harnesses") {
                ForEach(model.coverage) { row in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack(spacing: 6) {
                            StatusDot(tone: row.gap ? .warning : (row.toolGuard ? .good : .neutral),
                                      label: row.gap ? "Not covered" : "Covered")
                            Text(verbatim: row.title).textStyle(.rowTitle)
                            Spacer(minLength: 6)
                            capability("Pins", row.pins)
                            capability("Tool guard", row.toolGuard)
                            capability("Moves", row.directoryMoves)
                        }
                        Text(verbatim: row.note).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
                        Text(verbatim: "\(row.pinnedSessions) pinned \(row.pinnedSessions == 1 ? "session" : "sessions") · \(row.unpinnedProcesses) running unpinned · \(row.toolEvents) tool calls · \(row.refusals) refused")
                            .textStyle(.caption)
                    }
                    .padding(.vertical, 2)
                }
                if let hooks = model.hooksLayer, !hooks.problem.isEmpty {
                    Label { Text(verbatim: displaySafe(hooks.problem)) } icon: { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                        .textStyle(.caption)
                }
                Text("A running process with no pin started before the hooks were installed or failed to pin; restart it to cover it. `orch status` shows each harness's installed hooks.")
                    .textStyle(.caption)
            }
            Card("Enforcement") {
                if let state = enforcement.state {
                    ForEach(state.switches) { EnforcementRow(layer: $0, model: enforcement) }
                } else if let error = enforcement.lastError {
                    Text(verbatim: error).textStyle(.supporting)
                } else {
                    ProgressView().controlSize(.small)
                }
            }
        }
        .enforcementConfirmation(enforcement)
        .task { await enforcement.load() }
    }

    private func capability(_ name: String, _ on: Bool) -> some View {
        Chip(text: name, tone: on ? .good : .neutral, symbol: on ? "checkmark" : "xmark")
    }
}

// MARK: Sources

/// Stored sources, quarantine first, each with Retag.
struct SourcesEditor: View {
    let model: SpacesEditorModel
    @State private var filter = ""
    @State private var pending: (source: EditorSource, space: String)?

    var body: some View {
        Card("Sources") {
            TextField("Filter", text: $filter, prompt: Text("Filter by address")).textFieldStyle(.roundedBorder).labelsHidden()
        } content: {
            if let error = model.sourcesError { Text(verbatim: error).font(.caption).foregroundStyle(.orange) }
            let rows = shown
            if rows.isEmpty { Text("No sources.").textStyle(.supporting) }
            ForEach(rows) { source in
                HStack(spacing: 8) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(verbatim: displaySafe(source.uri)).textStyle(.code).lineLimit(1).truncationMode(.middle)
                        Text(verbatim: "\(displaySafe(source.sourceType)) · \(source.chunks) chunks").textStyle(.caption)
                    }
                    Spacer(minLength: 6)
                    if !source.space.isEmpty { Chip(text: displaySafe(source.space), tone: .warning) }
                    Menu("Retag") {
                        ForEach(model.retagTargets.filter { $0 != source.space }, id: \.self) { space in
                            Button(displaySafe(space)) { pending = (source, space) }
                        }
                    }
                    .fixedSize()
                    .disabled(model.retagging != nil)
                }
            }
            Text("Retag moves a source's stored content to another space, without re-embedding, and later refreshes keep it there. Showing \(rows.count) of \(model.sources.count).")
                .textStyle(.caption)
        }
        .confirmationDialog("Retag this source?", isPresented: Binding(get: { pending != nil }, set: { if !$0 { pending = nil } }),
                            titleVisibility: .visible) {
            if let p = pending {
                Button("Move to \(displaySafe(p.space))") { Task { await model.retag(p.source, to: p.space) } }
            }
        } message: {
            if let p = pending { Text(verbatim: model.retagWarning(p.source, to: p.space)) }
        }
    }

    private var shown: [EditorSource] {
        let f = filter.trimmingCharacters(in: .whitespaces).lowercased()
        let rows = f.isEmpty ? model.sources : model.sources.filter { $0.uri.lowercased().contains(f) }
        return Array(rows.prefix(300))
    }
}
