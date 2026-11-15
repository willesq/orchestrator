import OrchKit
import SwiftUI

/// The main window's layout when it is narrow: one column of what a developer
/// watches beside the terminal their agents run in (orchd's health, the PRs
/// waiting on them, the agents at work) with every pane a click away. It
/// replaces the sidebar layout, which clips below its width.
struct CompanionView: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let tasks: TasksModel
    let window: WindowControl
    /// The panes the strip lists, as the sidebar does: no Spaces while
    /// spaces are off.
    let panes: [Pane]
    /// A pane's badge, as the sidebar shows it; Spaces' is its badge (SP-055).
    let status: (Pane) -> SidebarStatus
    /// Shows a pane in the full layout.
    let open: (Pane) -> Void
    let openPR: (String) -> Void
    let openTask: (String) -> Void
    let newTask: () -> Void

    @State private var cancelling: TaskRecord?

    private let shownPRs = 6
    private let shownRecent = 3

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    Card("Needs you") { needsYou }
                    Card("Agents", accessory: {
                        Button("New task…", action: newTask)
                            .buttonStyle(.link)
                            .help("Dispatch work to a background agent")
                    }, content: { agents })
                }
                .padding(12)
            }
            Divider()
            paneStrip
        }
        .navigationTitle("Orch")
        // The Tasks pane follows the stream while it shows; this column is
        // the only task list on screen when the window is narrow.
        .task(id: tasks.filter) { await tasks.follow() }
        .confirmationDialog("Cancel this task?", isPresented: Binding(get: { cancelling != nil },
                                                                      set: { if !$0 { cancelling = nil } }),
                            presenting: cancelling) { task in
            Button("Cancel task", role: .destructive) {
                tasks.selectedID = task.id
                Task { await tasks.cancelSelected() }
            }
            Button("Keep running", role: .cancel) {}
        } message: { task in
            Text(verbatim: displaySafe(task.headline))
        }
    }

    private var header: some View {
        HStack(spacing: 10) {
            HealthMark(health: monitor.health, size: 24)
            Text(monitor.health.summary)
                .font(.callout.weight(.semibold))
                .lineLimit(2)
            Spacer(minLength: 4)
            Button { window.togglePinned() } label: {
                Image(systemName: window.isPinned ? "pin.fill" : "pin")
            }
            .help(window.isPinned ? "Stop keeping Orch above other windows" : "Keep Orch above other windows")
            .accessibilityLabel(window.isPinned ? "Unpin" : "Pin above other windows")
            if !window.isDocked {
                Button { window.dock() } label: { Image(systemName: "pip.enter") }
                    .help("Dock to the screen's edge (⌘⇧D)")
                    .accessibilityLabel("Dock to the screen's edge")
            }
            Button { window.expand() } label: { Image(systemName: "pip.exit") }
                .help("Expand to the full window (⌘⇧D)")
                .accessibilityLabel("Expand")
        }
        .buttonStyle(.borderless)
        .padding(.horizontal, 12)
        .padding(.vertical, 10)
    }

    @ViewBuilder private var needsYou: some View {
        let prs = prMonitor.needsYou
        if prMonitor.list == nil {
            Text(prMonitor.lastError.map { "Could not load PRs: \($0)" } ?? "Loading PRs…").textStyle(.caption)
        } else if prs.isEmpty {
            Label("Nothing needs you", systemImage: "checkmark.circle")
                .font(.callout)
                .foregroundStyle(Color.secondaryText)
        } else {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(prs.prefix(shownPRs)) { pr in
                    CompanionPRRow(pr: pr) { openPR(pr.url) }
                }
                if prs.count > shownPRs {
                    Button("\(prs.count - shownPRs) more in GitHub") { open(.github) }.buttonStyle(.link)
                }
            }
        }
    }

    @ViewBuilder private var agents: some View {
        let active = tasks.tasks.filter(\.isUnsettled)
        let recent = tasks.tasks.filter { !$0.isUnsettled }.prefix(shownRecent)
        if !tasks.loaded {
            Text(tasks.lastError.map { "Could not load tasks: \($0)" } ?? "Loading tasks…").textStyle(.caption)
        } else {
            VStack(alignment: .leading, spacing: 8) {
                if active.isEmpty {
                    Label("No agent is working", systemImage: "moon.zzz")
                        .font(.callout)
                        .foregroundStyle(Color.secondaryText)
                }
                ForEach(active) { task in
                    CompanionTaskRow(task: task, acting: tasks.acting == task.id,
                                     open: { openTask(task.id) }, cancel: { cancelling = task })
                }
                if !recent.isEmpty {
                    Text("Recent").textStyle(.caption).padding(.top, 4)
                    ForEach(Array(recent)) { task in
                        CompanionTaskRow(task: task, acting: false, open: { openTask(task.id) }, cancel: nil)
                    }
                }
                if let message = tasks.actionMessage {
                    Text(verbatim: displaySafe(message)).textStyle(.caption)
                }
            }
        }
    }

    /// Every pane as an icon with its badge; a click expands the window to it.
    private var paneStrip: some View {
        HStack(spacing: 0) {
            ForEach(panes) { pane in
                Button { open(pane) } label: {
                    Image(systemName: pane.symbol)
                        .frame(maxWidth: .infinity, minHeight: 28)
                        .overlay(alignment: .topTrailing) { badge(status(pane)) }
                        .contentShape(Rectangle())
                }
                .buttonStyle(.borderless)
                .help("Open \(pane.title)")
                .accessibilityLabel(pane.title)
                .accessibilityValue(badgeLabel(status(pane)))
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
    }

    /// What a badge says, for VoiceOver, which does not read the dot.
    private func badgeLabel(_ status: SidebarStatus) -> String {
        switch status {
        case .none: ""
        case .count(_, _, let label), .dot(_, let label): label
        }
    }

    @ViewBuilder private func badge(_ status: SidebarStatus) -> some View {
        switch status {
        case .none:
            EmptyView()
        case .count(_, let tone, let label), .dot(let tone, let label):
            StatusDot(tone: tone, label: label, size: 7).offset(x: -6, y: 2)
        }
    }
}

private struct CompanionPRRow: View {
    let pr: PullRequest
    let open: () -> Void

    var body: some View {
        Button(action: open) {
            HStack(alignment: .top, spacing: 8) {
                Image(systemName: pr.attention?.symbolName ?? "circle")
                    .foregroundStyle(tint)
                    .frame(width: 16)
                VStack(alignment: .leading, spacing: 1) {
                    Text(verbatim: displaySafe(pr.title)).font(.callout).lineLimit(2)
                    Text(verbatim: displaySafe("\(pr.repo) · \(pr.attention?.label ?? "")"))
                        .textStyle(.caption)
                        .lineLimit(1)
                }
                Spacer(minLength: 0)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help("Open in the GitHub tab")
        .contextMenu {
            if let url = pr.webURL {
                Button("Open on GitHub") { NSWorkspace.shared.open(url) }
            }
        }
    }

    private var tint: Color {
        switch pr.attention {
        case .ciFailed, .changesRequested, .conflicts: .red
        case .reviewRequested: .blue
        case .readyToMerge: .green
        case nil: .secondary
        }
    }
}

private struct CompanionTaskRow: View {
    let task: TaskRecord
    let acting: Bool
    let open: () -> Void
    /// nil for a task that has ended.
    let cancel: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Button(action: open) {
                HStack(alignment: .top, spacing: 8) {
                    indicator.frame(width: 16, height: 16)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(verbatim: displaySafe(task.headline.isEmpty ? task.id : task.headline))
                            .font(.callout)
                            .lineLimit(2)
                        HStack(spacing: 6) {
                            StatusBadge(status: task.status)
                            Text(verbatim: displaySafe(task.agent)).textStyle(.caption).lineLimit(1)
                        }
                    }
                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(task.run?.label ?? "Open in Tasks")
            if acting {
                ProgressView().controlSize(.small)
            } else if let cancel, task.canCancel {
                Button(action: cancel) { Image(systemName: "stop.circle") }
                    .buttonStyle(.borderless)
                    .help("Cancel this task")
                    .accessibilityLabel("Cancel task")
            }
        }
    }

    @ViewBuilder private var indicator: some View {
        switch (task.status, task.run?.state) {
        case (_, "stalled"?):
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
        case ("in_progress", _):
            ProgressView().controlSize(.mini)
        case ("pending", _):
            Image(systemName: "clock").foregroundStyle(Color.secondaryText)
        case ("complete", _):
            Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
        case ("failed", _), ("interrupted", _):
            Image(systemName: "xmark.circle.fill").foregroundStyle(.red)
        default:
            Image(systemName: "circle").foregroundStyle(Color.secondaryText)
        }
    }
}
