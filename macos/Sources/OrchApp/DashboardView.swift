import OrchKit
import SwiftUI

struct DashboardView: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let restart: RestartModel
    let docs: DocsModel
    let safetyNet: SafetyNetModel
    let tasks: TasksModel
    let reviews: ReviewMonitor
    let sources: RegisteredSourcesModel
    /// Shows another pane, for the summary chips.
    let open: (Pane) -> Void
    /// Shows the Safety Net part of Settings.
    let openSafetyNet: () -> Void
    /// Opens what a timeline entry records.
    let follow: (ActivityTarget) -> Void

    @State private var confirmRestartAll = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
                header
                summary
                // What needs the user comes before the daemon's internals.
                NeedsYouSection(monitor: prMonitor)
                    .padding(Metrics.cardPadding)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .cardSurface()
                ActivityTimeline(tasks: tasks, reviews: reviews, docs: docs, sources: sources, follow: follow)
                subsystems
            }
            .padding(Metrics.pagePadding)
        }
        .navigationTitle("Dashboard")
        .toolbar {
            Button {
                Task {
                    await monitor.refresh()
                    await prMonitor.refresh()
                }
            } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
            .help("Check orchd and the PRs that need you now")
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            HealthMark(health: monitor.health, size: 44)
            VStack(alignment: .leading, spacing: 2) {
                Text("orchd").font(.title2.bold())
                Text(monitor.health.summary).textStyle(.supporting)
            }
        }
        .accessibilityElement(children: .combine)
    }

    /// One chip per area that can need the user, each opening its tab.
    private var summary: some View {
        FlowLayout(spacing: 10) {
            SummaryChip(symbol: Pane.docs.symbol, title: "Docs", value: docsValue.text, tone: docsValue.tone) { open(.docs) }
            SummaryChip(symbol: SettingsSection.safetyNet.symbol, title: "Safety Net", value: safetyValue.text, tone: safetyValue.tone,
                        action: openSafetyNet)
            SummaryChip(symbol: "arrow.triangle.pull", title: "PRs", value: prValue.text, tone: prValue.tone) { open(.github) }
        }
    }

    private var docsValue: (text: String, tone: Tone) {
        guard let catalog = docs.catalog else { return ("…", .neutral) }
        let failing = catalog.failingRepos.count
        if failing > 0 { return ("\(failing) failing", .warning) }
        return catalog.needsAttention ? ("needs attention", .warning) : ("ok", .good)
    }

    private var safetyValue: (text: String, tone: Tone) {
        guard let state = safetyNet.state else { return ("…", .neutral) }
        return (state.badge.label, SafetyNetView.tone(state.badge.tone))
    }

    private var prValue: (text: String, tone: Tone) {
        guard prMonitor.list != nil else { return ("…", .neutral) }
        let n = prMonitor.needsYou.count
        return n == 0 ? ("none need you", .good) : ("\(n) need\(n == 1 ? "s" : "") you", .accent)
    }

    /// Every unit at a glance; the System tab has the detail. A failed unit
    /// turns its chip red and says why on hover.
    @ViewBuilder private var subsystems: some View {
        if let status = monitor.health.status {
            Card("Subsystems") {
                HStack(spacing: 12) {
                    Button("Details") { open(.system) }
                        .buttonStyle(.link)
                    Button("Restart all…") { confirmRestartAll = true }
                        .disabled(restart.restarting != nil)
                        .confirmationDialog("Restart every subsystem?", isPresented: $confirmRestartAll) {
                            Button("Restart all", role: .destructive) { Task { await restart.restart(nil) } }
                        } message: {
                            Text("In-flight indexing and queued work pause while each unit restarts.")
                        }
                }
            } content: {
                VStack(alignment: .leading, spacing: 6) {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 170), spacing: 6, alignment: .leading)],
                              alignment: .leading, spacing: 6) {
                        ForEach(status.subsystems) { unit in
                            SubsystemChip(unit: unit, restart: restart)
                        }
                    }
                    if let message = restart.lastMessage {
                        Text(message)
                            .font(.caption)
                            .foregroundStyle(restart.lastFailed ? Color.red : Color.secondaryText)
                    }
                }
            }
        }
    }
}

/// One unit as a chip: its state dot and name, with Restart on right-click.
private struct SubsystemChip: View {
    let unit: Subsystem
    let restart: RestartModel

    var body: some View {
        HStack(spacing: 7) {
            if restart.restarting == unit.name {
                ProgressView().controlSize(.mini).frame(width: 8, height: 8)
            } else {
                StatusDot(tone: tone, label: stateText)
            }
            Text(verbatim: unit.name)
                .font(.callout)
                .lineLimit(1)
                .foregroundStyle(unit.isFailed ? Color.red : Color.primary)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 9)
        .padding(.vertical, 6)
        .background(unit.isFailed ? Color.red.opacity(0.1) : Color.primary.opacity(0.04),
                    in: RoundedRectangle(cornerRadius: 6, style: .continuous))
        .help(unit.error.isEmpty ? "\(unit.detail) · \(stateText)" : unit.error)
        .contextMenu {
            Button { Task { await restart.restart(unit.name) } } label: { Text(verbatim: "Restart \(unit.name)") }
                .disabled(restart.restarting != nil)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(verbatim: "\(unit.name), \(stateText)"))
        .accessibilityAction(named: "Restart") { Task { await restart.restart(unit.name) } }
    }

    private var stateText: String { unit.isFailed ? "failed" : unit.state }

    private var tone: Tone {
        if unit.isFailed { return .bad }
        return unit.state == "running" ? .good : .neutral
    }
}

/// A tappable summary of one area, Material's assist chip in macOS dress.
private struct SummaryChip: View {
    let symbol: String
    let title: String
    let value: String
    let tone: Tone
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 8) {
                Image(systemName: symbol).foregroundStyle(tone.color)
                VStack(alignment: .leading, spacing: 0) {
                    Text(title).textStyle(.caption)
                    Text(value).font(.callout.weight(.semibold))
                }
                Image(systemName: "chevron.right").font(.caption2.weight(.semibold)).foregroundStyle(Color.tertiaryText)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .contentShape(Rectangle())
            .cardSurface(radius: 8)
        }
        .buttonStyle(.plain)
        .help("Open \(title)")
        .accessibilityLabel("\(title): \(value)")
        .accessibilityHint("Opens the \(title) tab")
    }
}

/// One unit: its state, what it does or its last error, and a menu with
/// Restart. A menu rather than a button per row keeps nine identical buttons
/// from competing with the content.
struct SubsystemRow: View {
    let unit: Subsystem
    let restart: RestartModel
    /// The heartbeat age, when the row shows one.
    var beat: String?
    /// Lay the detail beside the name instead of under it.
    var compact = false

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            StatusDot(tone: tone, label: stateText)
            if compact {
                Text(verbatim: unit.name).font(.body).frame(width: 170, alignment: .leading)
                detail.lineLimit(1).truncationMode(.tail)
            } else {
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: unit.name).font(.body)
                    detail.lineLimit(2)
                }
            }
            Spacer(minLength: 8)
            if unit.restarts > 0 {
                Text("\(unit.restarts) restart\(unit.restarts == 1 ? "" : "s")")
                    .textStyle(.caption)
                    .monospacedDigit()
            }
            Text(verbatim: beat ?? stateText)
                .textStyle(.caption)
                .monospacedDigit()
                .frame(minWidth: 56, alignment: .trailing)
            if restart.restarting == unit.name {
                ProgressView().controlSize(.small).frame(width: 28)
            } else {
                Menu {
                    restartButton
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .frame(width: 28)
                .disabled(restart.restarting != nil)
                .help("Actions for \(unit.name)")
                .accessibilityLabel("Actions for \(unit.name)")
            }
        }
        .contentShape(Rectangle())
        .contextMenu { restartButton }
        .accessibilityElement(children: .contain)
    }

    private var restartButton: some View {
        Button("Restart \(unit.name)") { Task { await restart.restart(unit.name) } }
            .disabled(restart.restarting != nil)
    }

    private var detail: some View {
        Text(verbatim: unit.error.isEmpty ? unit.detail : unit.error)
            .font(.caption)
            .foregroundStyle(unit.error.isEmpty ? Color.secondaryText : Color.red)
            .help(unit.error.isEmpty ? unit.detail : unit.error)
    }

    private var stateText: String { unit.isFailed ? "failed" : unit.state }

    private var tone: Tone {
        if unit.isFailed { return .bad }
        return unit.state == "running" ? .good : .neutral
    }
}
