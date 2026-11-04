import AppKit
import OrchKit
import SwiftUI

/// What one agent run did, live, read from its session's transcript. A summary
/// says what kind of run it was; each step is one line that opens to show the
/// whole of it. Risky commands stay in view however long the run gets.
struct AgentActivityFeed: View {
    let task: TaskRecord

    @Environment(PRLandingModel.self) private var landings
    @State private var expanded: Bool
    @State private var filter = TaskActivityFilter.all
    @State private var openSteps: Set<Int> = []

    init(task: TaskRecord) {
        self.task = task
        _expanded = State(initialValue: task.isUnsettled)
    }

    var body: some View {
        let log = landings.activity[task.id]
        VStack(alignment: .leading, spacing: 4) {
            DisclosureGroup(isExpanded: $expanded) {
                content(log).padding(.top, 6)
            } label: {
                Text(verbatim: log?.available == true ? log?.summary ?? "Activity" : "Activity")
                    .textStyle(.caption)
                    .accessibilityLabel(log?.available == true ? "Activity: \(log?.summary ?? "")" : "Activity")
            }
            if let log, log.pushed, PRHandoff.askedNotToPush(task) {
                Label("It ran git push, though its brief said not to. Check the PR's branch.",
                      systemImage: "exclamationmark.triangle.fill")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.red)
            }
        }
        .task(id: "\(task.id)|\(task.isUnsettled)") {
            await landings.followActivity(task.id, running: task.isUnsettled)
        }
    }

    @ViewBuilder
    private func content(_ log: TaskActivityLog?) -> some View {
        if let error = landings.activityErrors[task.id], log == nil {
            VStack(alignment: .leading, spacing: 4) {
                ErrorText("Could not read the session: \(displaySafe(error))")
                Button("Try again") { Task { await landings.refreshActivity(task.id) } }.controlSize(.small)
            }
        } else if let log {
            if !log.available {
                Text("No session to read yet: the agent has not started, or it runs on Codex, whose steps orch cannot read.")
                    .textStyle(.caption)
            } else if log.total == 0 {
                Text("The session has started, and has run no tools yet.").textStyle(.caption)
            } else {
                loaded(log)
            }
        } else {
            ProgressView().controlSize(.small)
        }
    }

    private func loaded(_ log: TaskActivityLog) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            ActivityRunLine(task: task, log: log)
            ActivityToolChips(tools: log.tools)
            if !log.files.isEmpty { ActivityFilesEdited(files: log.files) }
            if log.total > 3 {
                Picker("Show", selection: $filter) {
                    ForEach(TaskActivityFilter.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .controlSize(.small)
            }
            steps(log)
        }
    }

    private func steps(_ log: TaskActivityLog) -> some View {
        let shown = log.steps.filter(filter.includes)
        return Group {
            if shown.isEmpty {
                Text(verbatim: filter == .attention ? "Nothing failed, and nothing risky ran." : "No steps of this kind.")
                    .textStyle(.caption)
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 0) {
                            ForEach(shown) { step in
                                ActivityStepRow(step: step, startedAt: log.startedAt, open: binding(for: step)).id(step.id)
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .frame(maxHeight: 260)
                    .onChange(of: log.total) { _, _ in
                        if task.isUnsettled, let last = shown.last { proxy.scrollTo(last.id, anchor: .bottom) }
                    }
                }
                if log.total > log.steps.count {
                    Text(verbatim: "Showing \(log.steps.count) of \(log.total) steps: the newest, and the newest risky ones.")
                        .textStyle(.caption)
                }
            }
        }
    }

    private func binding(for step: TaskActivityStep) -> Binding<Bool> {
        Binding(get: { openSteps.contains(step.id) }, set: { isOpen in
            if isOpen { openSteps.insert(step.id) } else { openSteps.remove(step.id) }
        })
    }
}

/// How long the run took and on what, for telling at a glance what it cost.
private struct ActivityRunLine: View {
    let task: TaskRecord
    let log: TaskActivityLog

    var body: some View {
        if !facts.isEmpty {
            Text(verbatim: displaySafe(facts.joined(separator: " · "))).textStyle(.caption)
        }
    }

    private var facts: [String] {
        var out: [String] = []
        if task.durationMS > 0 {
            out.append(TaskActivityLog.duration(TimeInterval(task.durationMS) / 1000))
        } else if let span = log.span {
            out.append(TaskActivityLog.duration(span))
        }
        if task.turns > 0 { out.append("\(task.turns) turn\(task.turns == 1 ? "" : "s")") }
        out.append(contentsOf: task.models)
        return out
    }
}

/// Each tool the run used and how often.
private struct ActivityToolChips: View {
    let tools: [TaskActivityLog.Tool]

    var body: some View {
        FlowLayout(spacing: 4) {
            ForEach(tools) { tool in
                Chip(text: displaySafe("\(tool.label) \(tool.count)"))
            }
        }
    }
}

/// The files the run edited, with the lines each edit wrote and replaced.
private struct ActivityFilesEdited: View {
    let files: [TaskActivityLog.File]

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("Files edited").textStyle(.caption)
            ForEach(files.prefix(8)) { file in
                HStack(spacing: 6) {
                    Text(verbatim: displaySafe(TaskActivityLog.shortPath(file.path)))
                        .textStyle(.codeCaption)
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .help(displaySafe(file.path))
                    Spacer(minLength: 4)
                    ActivityLineCounts(additions: file.additions, deletions: file.deletions)
                }
            }
            if files.count > 8 {
                Text(verbatim: "and \(files.count - 8) more").textStyle(.caption)
            }
        }
    }
}

private struct ActivityLineCounts: View {
    let additions: Int
    let deletions: Int

    var body: some View {
        HStack(spacing: 4) {
            if additions > 0 { Text(verbatim: "+\(additions)").foregroundStyle(.green) }
            if deletions > 0 { Text(verbatim: "−\(deletions)").foregroundStyle(.red) }
        }
        .font(.caption.monospacedDigit())
    }
}

/// One step: a single line that opens to show the whole of it. The closed line
/// is not selectable: selecting text lets it re-lay itself out past its limit,
/// over the rows below. Copying happens once it is open.
private struct ActivityStepRow: View {
    let step: TaskActivityStep
    let startedAt: Date?
    @Binding var open: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                if step.hasMore { open.toggle() }
            } label: {
                header.contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(!step.hasMore)
            .accessibilityLabel(displaySafe("\(step.tool): \(step.title)"))
            .accessibilityValue(step.failed ? "failed" : step.risk)
            .help(step.hasMore ? (open ? "Hide the details" : "Show the details") : displaySafe(step.title))
            if open, step.hasMore {
                ActivityStepDetail(step: step).padding(.leading, 20)
            }
        }
        .padding(.vertical, 3)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Image(systemName: step.symbolName)
                .imageScale(.small)
                .frame(width: 14)
                .foregroundStyle(iconColor)
            if !step.risk.isEmpty { Chip(text: step.risk, tone: .bad) }
            if step.failed { Chip(text: "failed", tone: .bad) }
            Text(verbatim: displaySafe(rowTitle))
                .textStyle(.codeCaption)
                .lineLimit(1)
                .truncationMode(.tail)
            if step.commandTotal > 1 {
                Text(verbatim: "+\(step.commandTotal - 1)").textStyle(.caption)
            }
            Spacer(minLength: 6)
            ActivityLineCounts(additions: step.additions, deletions: step.deletions)
            if step.duration >= 1 {
                Text(verbatim: TaskActivityLog.duration(step.duration)).textStyle(.caption).lineLimit(1).fixedSize()
            } else if !step.finished, step.kind == "run" {
                Text("no result").textStyle(.caption).lineLimit(1).fixedSize()
            }
            if step.hasMore {
                Image(systemName: open ? "chevron.down" : "chevron.right")
                    .imageScale(.small)
                    .foregroundStyle(Color.secondaryText)
                    .frame(width: 10)
            }
        }
    }

    private var rowTitle: String {
        step.kind == "edit" || step.tool == "Read" ? TaskActivityLog.shortPath(step.title) : step.title
    }

    private var iconColor: Color {
        !step.risk.isEmpty || step.failed ? .red : .secondaryText
    }
}

/// An open step: its commands one per line, what came back, and Copy.
private struct ActivityStepDetail: View {
    let step: TaskActivityStep

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if step.commands.count > 1 {
                VStack(alignment: .leading, spacing: 2) {
                    ForEach(Array(step.commands.enumerated()), id: \.offset) { _, command in
                        codeText(command)
                    }
                    if step.commandTotal > step.commands.count {
                        Text(verbatim: "and \(step.commandTotal - step.commands.count) more, in the session itself")
                            .textStyle(.caption)
                    }
                }
            } else if !step.detail.isEmpty {
                codeText(step.detail)
            } else {
                codeText(step.text)
            }
            if !step.output.isEmpty {
                Text(step.failed ? "Error" : "Output").textStyle(.caption)
                codeText(step.output)
                    .padding(6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.secondary.opacity(0.08), in: RoundedRectangle(cornerRadius: 4))
            }
            HStack(spacing: 8) {
                Button("Copy") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(step.copyText, forType: .string)
                }
                .controlSize(.small)
                if let at = step.at {
                    Text(at, style: .time).textStyle(.caption)
                }
                if !step.tool.isEmpty {
                    Text(verbatim: displaySafe(step.tool)).textStyle(.caption)
                }
            }
        }
    }

    /// Selectable, so it may not be limited: it takes the height it needs.
    private func codeText(_ text: String) -> some View {
        Text(verbatim: displaySafeLines(text))
            .textStyle(.codeCaption)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}
