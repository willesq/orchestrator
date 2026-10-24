import OrchKit
import SwiftUI

enum Pane: String, CaseIterable, Identifiable {
    case dashboard, memory, tasks, github

    var id: String { rawValue }

    var title: String {
        switch self {
        case .dashboard: "Dashboard"
        case .memory: "Memory"
        case .tasks: "Tasks"
        case .github: "GitHub"
        }
    }

    var symbol: String {
        switch self {
        case .dashboard: "gauge.with.dots.needle.33percent"
        case .memory: "brain.head.profile"
        case .tasks: "checklist"
        case .github: "point.3.connected.trianglepath.dotted"
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
            case .memory:
                MemoryView(search: search, sources: sources)
            case .tasks:
                TasksView(model: tasks, dispatch: dispatch)
            case .github:
                GitHubView(model: github)
            }
        }
        .frame(minWidth: 760, minHeight: 480)
    }
}
