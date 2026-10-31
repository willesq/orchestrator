import AppKit
import OrchKit
import SwiftUI

struct TasksView: View {
    @Bindable var model: TasksModel
    let dispatch: DispatchModel
    let navigator: Navigator

    @State private var showDispatch = false

    var body: some View {
        Group {
            if model.loaded && model.tasks.isEmpty {
                // Nothing to select, so no detail pane: one empty state with
                // the action that fills the list.
                VStack(spacing: 0) {
                    if let error = model.lastError {
                        Text(error).font(.caption).foregroundStyle(.red).padding(8)
                    }
                    if model.filter == .all {
                        EmptyState(symbol: "paperplane", title: "No tasks yet",
                                   message: "Dispatch a task and a background agent works on it; its progress and result show up here.",
                                   actionTitle: "Dispatch task", action: { showDispatch = true })
                    } else {
                        EmptyState(symbol: "line.3.horizontal.decrease.circle", title: "No \(model.filter.label.lowercased()) tasks",
                                   message: "No task has this status.",
                                   actionTitle: "Show all tasks", action: { model.filter = .all })
                    }
                }
            } else {
                HSplitView {
                    list.frame(minWidth: 280, idealWidth: 320)
                    Group {
                        if let task = model.selected {
                            TaskDetail(task: task, model: model)
                        } else {
                            EmptyState(symbol: "list.bullet.rectangle", title: "No task selected",
                                       message: "Select a task to see its body, result and outcome.")
                        }
                    }
                    .frame(minWidth: 360, maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .navigationTitle("Tasks")
        .toolbar {
            Picker("Status", selection: $model.filter) {
                ForEach(TaskStatusFilter.allCases) { Text("Status: \($0.label)").tag($0) }
            }
            .help("Show tasks with this status")
            Button { Task { await model.refresh() } } label: { Label("Refresh", systemImage: "arrow.clockwise") }
                .help(model.live ? "The list updates live; reload it anyway" : "Reload the task list")
            Button { showDispatch = true } label: { Label("Dispatch task", systemImage: "paperplane") }
                .help("Dispatch a task to a background agent")
        }
        .sheet(isPresented: $showDispatch) { DispatchSheet(model: dispatch) }
        .task(id: navigator.dispatchRequested) {
            guard navigator.dispatchRequested else { return }
            navigator.dispatchRequested = false
            showDispatch = true
        }
        // Follows orchd's task stream while the tab is shown; a new filter
        // restarts it, and its first message replaces the list.
        .task(id: model.filter) { await model.follow() }
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
                        if task.run?.isStalled == true {
                            Chip(text: "stalled", tone: .bad)
                                .help("Nothing is running this task; open it to retry or cancel")
                        }
                        Text(verbatim: task.agent).font(.caption).foregroundStyle(Color.secondaryText)
                        if let rating = task.rating {
                            Text(rating.label).font(.caption).foregroundStyle(Color.secondaryText)
                        }
                        Spacer()
                        Text(verbatim: task.updatedAt).font(.caption2).foregroundStyle(Color.tertiaryText)
                    }
                }
                .tag(task.id)
            }
        }
    }
}

private struct StatusBadge: View {
    let status: String

    var body: some View {
        Chip(text: status.replacingOccurrences(of: "_", with: " "), tone: tone)
    }

    private var tone: Tone {
        switch status {
        case "complete": .good
        case "failed", "interrupted": .bad
        case "in_progress": .accent
        default: .neutral
        }
    }
}

private struct TaskDetail: View {
    let task: TaskRecord
    let model: TasksModel

    @State private var confirmCancel = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack {
                    StatusBadge(status: task.status)
                    Text(verbatim: task.id).font(.caption.monospaced()).textSelection(.enabled)
                    Spacer()
                    Text(verbatim: task.agent).foregroundStyle(Color.secondaryText)
                }
                if let run = task.run { runInfo(run) }
                if task.canRetry || task.canCancel { actions }
                facts
                section("Task", task.body)
                if !task.result.isEmpty { section("Result", task.result) }
                if !task.prs.isEmpty { prs }
                if task.isFinished { rating }
            }
            .padding(16)
        }
    }

    /// Whether anything is working on the task, refreshed by the stream.
    private func runInfo(_ run: TaskRunInfo) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Label(run.label, systemImage: run.isStalled ? "exclamationmark.triangle" : run.state == "running" ? "bolt" : "clock")
                .foregroundStyle(run.isStalled ? AnyShapeStyle(.red) : AnyShapeStyle(.primary))
                .font(.headline)
            Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 4) {
                if let started = run.startedAt {
                    GridRow {
                        Text("Started").foregroundStyle(Color.secondaryText)
                        Text(started, format: .relative(presentation: .named))
                    }
                }
                if let active = run.lastActivity {
                    GridRow {
                        Text("Last activity").foregroundStyle(Color.secondaryText)
                        Text(active, format: .relative(presentation: .named))
                    }
                }
                if !run.sessionID.isEmpty { row("Session", run.sessionID) }
                if !run.cwd.isEmpty { row("Directory", run.cwd) }
            }
            .font(.callout)
        }
    }

    private var actions: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                if task.canRetry {
                    Button("Retry") { Task { await model.retrySelected() } }
                        .help("Run the task again; its session resumes where it left off")
                }
                if task.canCancel {
                    Button("Cancel…", role: .destructive) { confirmCancel = true }
                        .help("Stop this task")
                        .confirmationDialog("Cancel this task?", isPresented: $confirmCancel) {
                            Button("Cancel Task", role: .destructive) { Task { await model.cancelSelected() } }
                        } message: {
                            Text("A running session is stopped. Work it already pushed stays.")
                        }
                }
                if let command = task.run?.resumeCommand, !command.isEmpty {
                    Button("Copy Resume Command") {
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString(command, forType: .string)
                    }
                    .help("Copy the command that opens this session in Claude Code: \(command)")
                }
            }
            .buttonStyle(.bordered)
            .disabled(model.acting != nil)
            if let message = model.actionMessage, model.acting == nil {
                Text(verbatim: message).font(.caption).foregroundStyle(Color.secondaryText)
            }
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
            Text(label).foregroundStyle(Color.secondaryText)
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
                .font(.caption).foregroundStyle(Color.secondaryText)
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
                    .font(.caption).foregroundStyle(Color.secondaryText)
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
                        Text(verbatim: "Routed to \(route.label)").font(.caption).foregroundStyle(Color.secondaryText)
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
