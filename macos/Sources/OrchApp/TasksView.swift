import AppKit
import OrchKit
import SwiftUI

struct TasksView: View {
    @Bindable var model: TasksModel
    let dispatch: DispatchModel

    @State private var showDispatch = false

    var body: some View {
        HSplitView {
            list.frame(minWidth: 280, idealWidth: 320)
            Group {
                if let task = model.selected {
                    TaskDetail(task: task, model: model)
                } else {
                    ContentUnavailableView("Select a task", systemImage: "list.bullet.rectangle")
                }
            }
            .frame(minWidth: 360, maxWidth: .infinity, maxHeight: .infinity)
        }
        .navigationTitle("Tasks")
        .toolbar {
            Picker("Status", selection: $model.filter) {
                ForEach(TaskStatusFilter.allCases) { Text($0.label).tag($0) }
            }
            .onChange(of: model.filter) { Task { await model.refresh() } }
            Button { Task { await model.refresh() } } label: { Label("Refresh", systemImage: "arrow.clockwise") }
            Button { showDispatch = true } label: { Label("Dispatch", systemImage: "paperplane") }
        }
        .sheet(isPresented: $showDispatch) { DispatchSheet(model: dispatch) }
        .task { if !model.loaded { await model.refresh() } }
    }

    private var list: some View {
        VStack(spacing: 0) {
            if let error = model.lastError {
                Text(error).font(.caption).foregroundStyle(.red).padding(8)
            }
            List(model.tasks, selection: $model.selectedID) { task in
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: task.headline.isEmpty ? task.id : task.headline).lineLimit(1)
                    HStack(spacing: 6) {
                        StatusBadge(status: task.status)
                        Text(verbatim: task.agent).font(.caption).foregroundStyle(.secondary)
                        if let rating = task.rating {
                            Text(rating.label).font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        Text(verbatim: task.updatedAt).font(.caption2).foregroundStyle(.tertiary)
                    }
                }
                .tag(task.id)
            }
            .overlay {
                if model.loaded && model.tasks.isEmpty {
                    ContentUnavailableView("No tasks", systemImage: "tray",
                                           description: Text("Dispatch one with the paper plane."))
                }
            }
        }
    }
}

private struct StatusBadge: View {
    let status: String

    var body: some View {
        Text(verbatim: status.replacingOccurrences(of: "_", with: " "))
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(color.opacity(0.2), in: .capsule)
            .foregroundStyle(color)
    }

    private var color: Color {
        switch status {
        case "complete": .green
        case "failed": .red
        case "in_progress": .blue
        default: .secondary
        }
    }
}

private struct TaskDetail: View {
    let task: TaskRecord
    let model: TasksModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack {
                    StatusBadge(status: task.status)
                    Text(verbatim: task.id).font(.caption.monospaced()).textSelection(.enabled)
                    Spacer()
                    Text(verbatim: task.agent).foregroundStyle(.secondary)
                }
                facts
                section("Task", task.body)
                if !task.result.isEmpty { section("Result", task.result) }
                if !task.prs.isEmpty { prs }
                if task.isFinished { rating }
            }
            .padding(16)
        }
    }

    private var facts: some View {
        Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 4) {
            if let route = task.route { row("Model", route.label) }
            if !task.models.isEmpty { row("Ran on", task.models.joined(separator: ", ")) }
            if task.durationMS > 0 { row("Took", Duration.milliseconds(task.durationMS).formatted(.units(width: .abbreviated))) }
            if task.turns > 0 { row("Turns", "\(task.turns)") }
            if task.costUSD > 0 { row("Cost", task.costUSD.formatted(.currency(code: "USD"))) }
            if !task.verification.isEmpty { row("Verification", task.verification) }
            if !task.prOutcome.isEmpty { row("PR outcome", task.prOutcome) }
            if !task.updatedAt.isEmpty { row("Updated", task.updatedAt) }
        }
        .font(.callout)
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary)
            Text(verbatim: value).textSelection(.enabled)
        }
    }

    private func section(_ title: String, _ text: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.headline)
            // Agent and user text: plain, never Markdown.
            Text(verbatim: text)
                .font(.callout.monospaced())
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(8)
                .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 6))
        }
    }

    private var prs: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("PRs").font(.headline)
            ForEach(task.prs, id: \.self) { raw in
                let url = PullRequest(url: raw, title: "", repo: "").webURL
                Button { if let url { NSWorkspace.shared.open(url) } } label: { Text(verbatim: raw) }
                    .buttonStyle(.link)
                    .disabled(url == nil)
            }
        }
    }

    private var rating: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Your rating").font(.headline)
            HStack {
                ForEach(TaskRating.allCases) { verdict in
                    Button(verdict.label) { Task { await model.rateSelected(verdict) } }
                        .buttonStyle(.bordered)
                        .tint(task.rating == verdict ? .accentColor : nil)
                        .disabled(model.rating != nil)
                }
            }
            Text("Ratings teach model routing which models do this kind of task well.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }
}

private struct DispatchSheet: View {
    @Bindable var model: DispatchModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Dispatch a task").font(.title2.bold())
            Form {
                TextField("Agent", text: $model.draft.agent, prompt: Text("dev"))
                Picker("Kind", selection: $model.draft.taskKind) {
                    Text("None (agent's own model)").tag("")
                    ForEach(DispatchDraft.taskKinds, id: \.self) { Text($0).tag($0) }
                }
                .onChange(of: model.draft.taskKind) { Task { await model.refreshRecommendation() } }
                TextField("Model", text: $model.draft.model, prompt: Text("provider/slug, overrides the kind"))
                    .onChange(of: model.draft.model) { Task { await model.refreshRecommendation() } }
                HStack {
                    TextField("Working directory", text: $model.draft.cwd, prompt: Text("/absolute/path (optional)"))
                    Button("Choose…") { chooseFolder() }
                }
            }
            if let route = model.recommendation?.route {
                let source = model.recommendation?.fromTable == true ? "learned" : "proposed"
                Text(verbatim: "Routing would use \(route.label) (\(source), \(route.confidence) confidence)")
                    .font(.caption).foregroundStyle(.secondary)
            }
            TextEditor(text: $model.draft.body)
                .font(.body.monospaced())
                .frame(minHeight: 180)
                .overlay(RoundedRectangle(cornerRadius: 4).stroke(.quaternary))
            if let problem = model.lastError {
                Text(problem).font(.callout).foregroundStyle(.red)
            }
            if let outcome = model.outcome {
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: "Queued \(outcome.id)").font(.callout.monospaced()).textSelection(.enabled)
                    if let route = outcome.route {
                        Text(verbatim: "Routed to \(route.label)").font(.caption).foregroundStyle(.secondary)
                    }
                    if !outcome.workerEnabled {
                        Text("No worker is enabled, so it waits until queue.worker.enabled is on.")
                            .font(.caption).foregroundStyle(.orange)
                    }
                }
            }
            HStack {
                Spacer()
                Button("Close") {
                    model.reset()
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
                // ⌘↩, not plain Return: Return in a text field must not start an agent.
                Button("Dispatch ⌘↩") { Task { await model.submit() } }
                    .keyboardShortcut(.return, modifiers: .command)
                    .disabled(model.isSubmitting || model.draft.problem != nil)
            }
        }
        .padding(20)
        .frame(width: 560)
        .onDisappear { model.reset() }
    }

    private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        if panel.runModal() == .OK, let url = panel.url { model.draft.cwd = url.path }
    }
}
