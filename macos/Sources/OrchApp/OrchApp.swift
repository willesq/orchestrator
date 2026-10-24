import OrchKit
import SwiftUI

@main
struct OrchApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            TrayMenu(
                monitor: delegate.model.monitor, prMonitor: delegate.model.prMonitor,
                loginItem: delegate.model.loginItem, control: delegate.model.control,
                openMainWindow: { delegate.showMainWindow() }
            )
        } label: {
            TrayLabel(monitor: delegate.model.monitor, prMonitor: delegate.model.prMonitor)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Everything the tray and the main window share: one connection to orchd
/// and the models built on it.
@MainActor
final class AppModel {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let notifier = Notifier()
    let loginItem = LoginItem()
    let control = DaemonControl()
    let restart: RestartModel
    let search: SearchModel
    let sources: SourcesModel
    let tasks: TasksModel
    let dispatch: DispatchModel
    let github: GitHubModel

    init() {
        let api: any DaemonAPI & MemoryAPI & ControlAPI & TasksAPI & GitHubAPI
        do {
            let client = try DaemonClient()
            Task.detached { try? await client.run() }
            api = client
        } catch {
            api = UnreachableDaemon(reason: error.localizedDescription)
        }
        let monitor = DaemonMonitor(api: api)
        let tasks = TasksModel(api: api)
        self.monitor = monitor
        self.tasks = tasks
        search = SearchModel(api: api)
        sources = SourcesModel(api: api)
        dispatch = DispatchModel(api: api) { await tasks.refresh() }
        restart = RestartModel(api: api) { await monitor.refresh() }
        prMonitor = PRMonitor(api: api)
        github = GitHubModel(api: api)
    }

    /// Starts polling; called once the app has finished launching, and not
    /// at all in a copy that hands off to one already running.
    func start() {
        monitor.start()
        prMonitor.onNewlyNeedingYou = { [notifier] prs in notifier.post(prs) }
        prMonitor.start()
    }
}

/// The menu bar icon: a bell with a count when PRs need the user, otherwise
/// orchd's health.
private struct TrayLabel: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor

    var body: some View {
        let icon = TrayIcon(health: monitor.health, needsYou: prMonitor.needsYou.count)
        HStack(spacing: 2) {
            switch icon.mark {
            case .brand:
                if let glyph = BrandGlyph.image {
                    Image(nsImage: glyph)
                } else {
                    Image(systemName: "brain")
                }
            case .symbol(let name):
                Image(systemName: name)
            }
            if let count = icon.count { Text(verbatim: "\(count)") }
        }
        .accessibilityLabel(monitor.health.summary)
    }
}

/// The menu bar glyph shipped in the bundle (from Resources/Brand/glyph.svg),
/// as a template image so macOS tints it for light and dark menu bars. nil
/// when running outside Orch.app, where the bundle has no resources.
enum BrandGlyph {
    static let image: NSImage? = {
        guard let url = Bundle.main.url(forResource: "MenuBarGlyph", withExtension: "svg"),
              let image = NSImage(contentsOf: url) else { return nil }
        image.size = NSSize(width: 18, height: 18)
        image.isTemplate = true
        return image
    }()
}

/// Stands in when the gRPC transport cannot even be built, so the tray still
/// shows the daemon as down instead of the app failing to launch.
private struct UnreachableDaemon: DaemonAPI, MemoryAPI, ControlAPI, TasksAPI, GitHubAPI {
    let reason: String

    private var failure: DaemonControl.ControlError { .failed(status: -1, output: reason) }

    func status() async throws -> DaemonStatus { throw failure }
    func listPRs() async throws -> PRList { throw failure }
    func search(_ query: SearchQuery) async throws -> SearchResult { throw failure }
    func listSources(type: String, limit: Int) async throws -> SourceList { throw failure }
    func restart(_ name: String?) async throws -> [String] { throw failure }
    func listTasks(status: TaskStatusFilter, limit: Int) async throws -> [TaskRecord] { throw failure }
    func task(id: String) async throws -> TaskRecord? { throw failure }
    func rate(id: String, _ rating: TaskRating) async throws { throw failure }
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome { throw failure }
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) { throw failure }
    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity { throw failure }
    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList { throw failure }
    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope { throw failure }
    func gitHubRepos(owner: String) async throws -> [GitHubRepo] { throw failure }
    func gitHubItems() async throws -> GitHubItemList { throw failure }
}
