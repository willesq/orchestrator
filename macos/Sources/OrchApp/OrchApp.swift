import OrchKit
import SwiftUI

@main
struct OrchApp: App {
    private let monitor: DaemonMonitor
    private let prMonitor: PRMonitor
    private let notifier = Notifier()
    private let loginItem = LoginItem()
    private let control = DaemonControl()
    private let restart: RestartModel
    private let search: SearchModel
    private let sources: SourcesModel
    private let tasks: TasksModel
    private let dispatch: DispatchModel

    init() {
        let api: any DaemonAPI & MemoryAPI & ControlAPI & TasksAPI
        do {
            let client = try DaemonClient()
            Task.detached { try? await client.run() }
            api = client
        } catch {
            api = UnreachableDaemon(reason: error.localizedDescription)
        }
        monitor = DaemonMonitor(api: api)
        search = SearchModel(api: api)
        sources = SourcesModel(api: api)
        let tasks = TasksModel(api: api)
        self.tasks = tasks
        dispatch = DispatchModel(api: api) { await tasks.refresh() }
        restart = RestartModel(api: api) { [monitor] in await monitor.refresh() }
        monitor.start()
        prMonitor = PRMonitor(api: api)
        prMonitor.onNewlyNeedingYou = { [notifier] prs in notifier.post(prs) }
        prMonitor.start()
    }

    var body: some Scene {
        Window("Orchestrator", id: MainWindowID.value) {
            MainWindow(monitor: monitor, prMonitor: prMonitor, restart: restart, search: search, sources: sources,
                       tasks: tasks, dispatch: dispatch)
        }
        .defaultLaunchBehavior(.suppressed)

        MenuBarExtra {
            TrayMenu(monitor: monitor, prMonitor: prMonitor, loginItem: loginItem, control: control)
        } label: {
            let icon = TrayIcon(health: monitor.health, needsYou: prMonitor.needsYou.count)
            HStack(spacing: 2) {
                Image(systemName: icon.symbolName)
                if let count = icon.count { Text("\(count)") }
            }
            .accessibilityLabel(monitor.health.summary)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Stands in when the gRPC transport cannot even be built, so the tray still
/// shows the daemon as down instead of the app failing to launch.
enum MainWindowID {
    static let value = "main"
}

private struct UnreachableDaemon: DaemonAPI, MemoryAPI, ControlAPI, TasksAPI {
    let reason: String

    private var failure: DaemonControl.ControlError { .failed(status: -1, output: reason) }

    func search(_ query: SearchQuery) async throws -> SearchResult { throw failure }
    func listSources(type: String, limit: Int) async throws -> SourceList { throw failure }
    func restart(_ name: String?) async throws -> [String] { throw failure }
    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] { throw failure }
    func task(id: String) async throws -> TaskRecord? { throw failure }
    func rate(id: String, _ rating: TaskRating) async throws { throw failure }
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome { throw failure }
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) { throw failure }
    func status() async throws -> DaemonStatus {
        throw DaemonControl.ControlError.failed(status: -1, output: reason)
    }

    func listPRs() async throws -> PRList {
        throw DaemonControl.ControlError.failed(status: -1, output: reason)
    }
}
