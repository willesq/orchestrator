import OrchKit
import SwiftUI

enum Pane: String, CaseIterable, Identifiable {
    case dashboard, system, memory, docs, sources, tasks, github, claude, safetyNet

    var id: String { rawValue }

    var title: String {
        switch self {
        case .dashboard: "Dashboard"
        case .system: "System"
        case .memory: "Memory"
        case .docs: "Docs"
        case .sources: "Sources"
        case .tasks: "Tasks"
        case .github: "GitHub"
        case .claude: "Claude"
        case .safetyNet: "Safety Net"
        }
    }

    var symbol: String {
        switch self {
        case .dashboard: "gauge.with.dots.needle.33percent"
        case .system: "cpu"
        case .memory: "brain.head.profile"
        case .docs: "doc.text.magnifyingglass"
        case .sources: "tray.full"
        case .tasks: "checklist"
        case .github: "point.3.connected.trianglepath.dotted"
        case .claude: "slider.horizontal.3"
        case .safetyNet: "checkmark.shield"
        }
    }
}

struct MainWindow: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let restart: RestartModel
    let search: SearchModel
    let sources: SourcesModel
    let tasks: TasksModel
    let dispatch: DispatchModel
    let github: GitHubModel
    let prs: PRsModel
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel
    let claude: SettingsModel
    @Bindable var navigator: Navigator

    private var pane: Pane {
        get { navigator.pane }
        nonmutating set { navigator.pane = newValue }
    }

    var body: some View {
        NavigationSplitView {
            List(Pane.allCases, selection: Binding(get: { pane }, set: { pane = $0 ?? .dashboard })) { item in
                SidebarRow(pane: item, status: sidebarStatus(item), selected: item == pane).tag(item)
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190)
            .safeAreaInset(edge: .bottom) {
                AppearancePicker()
                    .pickerStyle(.menu)
                    .controlSize(.small)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
            }
        } detail: {
            switch pane {
            case .dashboard:
                DashboardView(monitor: monitor, prMonitor: prMonitor, restart: restart, docs: docs, safetyNet: safetyNet,
                              open: { pane = $0 })
            case .system:
                SystemView(model: system, logs: logs, restart: restart)
            case .memory:
                MemoryView(search: search, sources: sources)
            case .docs:
                DocsView(model: docs)
            case .sources:
                SourcesView(model: registeredSources)
            case .tasks:
                TasksView(model: tasks, dispatch: dispatch)
            case .github:
                GitHubView(model: github, prs: prs, navigator: navigator)
            case .claude:
                ClaudeView(model: claude)
            case .safetyNet:
                SafetyNetView(model: safetyNet, settings: claude)
            }
        }
        .frame(minWidth: 760, minHeight: 480)
    }

    /// What a pane's sidebar row shows beside its name, from data the
    /// models already hold.
    private func sidebarStatus(_ pane: Pane) -> SidebarStatus {
        switch pane {
        case .docs:
            let failing = docs.catalog?.failingRepos.count ?? 0
            return failing > 0 ? .count(failing, .warning, "\(failing) repos failed to sync") : .none
        case .github:
            let n = prMonitor.needsYou.count
            return n > 0 ? .count(n, .accent, "\(n) need you") : .none
        case .safetyNet:
            guard let state = safetyNet.state else { return .none }
            let badge = state.badge
            return .dot(SafetyNetView.tone(badge.tone), badge.label)
        case .claude:
            let n = claude.changedOutside
            return n > 0 ? .dot(.warning, "\(n) edited outside orch") : .none
        default:
            return .none
        }
    }
}

/// A sidebar badge: a count, or a colored state dot.
enum SidebarStatus: Equatable {
    case none
    case count(Int, Tone, String)
    case dot(Tone, String)
}

private struct SidebarRow: View {
    let pane: Pane
    let status: SidebarStatus
    /// On the selection highlight a tinted badge would vanish, so it turns white.
    var selected = false

    var body: some View {
        HStack {
            Label(pane.title, systemImage: pane.symbol)
            Spacer(minLength: 4)
            switch status {
            case .none:
                EmptyView()
            case .count(let n, let tone, let label):
                Text(verbatim: n > 99 ? "99+" : "\(n)")
                    .font(.caption.weight(.semibold).monospacedDigit())
                    .padding(.horizontal, 6)
                    .padding(.vertical, 1)
                    .background(selected ? Color.white.opacity(0.25) : tone.color.opacity(0.22), in: Capsule())
                    .foregroundStyle(selected ? Color.white : tone.color)
                    .help(label)
                    .accessibilityLabel(label)
            case .dot(let tone, let label):
                // A symbol rather than a shape, so VoiceOver reads its label
                // with the row's name.
                Image(systemName: "circle.fill")
                    .font(.system(size: 8))
                    .foregroundStyle(tone.color)
                    .help(label)
                    .accessibilityLabel(label)
            }
        }
        .accessibilityElement(children: .combine)
    }
}
