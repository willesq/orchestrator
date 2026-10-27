import OrchKit
import SwiftUI

enum Pane: String, CaseIterable, Identifiable {
    case dashboard, system, memory, docs, sources, tasks, github, safetyNet

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
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel

    @AppStorage("pane") private var pane: Pane = .dashboard

    var body: some View {
        NavigationSplitView {
            List(Pane.allCases, selection: Binding(get: { pane }, set: { pane = $0 ?? .dashboard })) { item in
                Label(item.title, systemImage: item.symbol).tag(item)
            }
            .navigationSplitViewColumnWidth(min: 160, ideal: 180)
        } detail: {
            switch pane {
            case .dashboard:
                DashboardView(monitor: monitor, prMonitor: prMonitor, restart: restart)
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
                GitHubView(model: github)
            case .safetyNet:
                SafetyNetView(model: safetyNet)
            }
        }
        .frame(minWidth: 760, minHeight: 480)
    }
}
