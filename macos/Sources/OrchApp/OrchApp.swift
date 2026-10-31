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
    let reviews: ReviewMonitor
    let prs: PRsModel
    let navigator = Navigator()
    /// Brings the main window forward; set by the app delegate.
    var showWindow: (@MainActor () -> Void)?
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel
    let claude: SettingsModel
    /// Keeps the sidebar's badges current between visits to their tabs.
    private(set) var badges: PeriodicRefresh?

    init() {
        let api: any DaemonAPI & MemoryAPI & ControlAPI & TasksAPI & GitHubAPI & PRReviewAPI & SystemAPI & CatalogAPI
            & SafetyNetAPI & SettingsAPI
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
        let reviews = ReviewMonitor(api: api)
        self.reviews = reviews
        prs = PRsModel(api: api, reviews: reviews, taskLookup: { [api] id in try await api.task(id: id) })
        system = SystemModel(api: api, loadReport: { try await DaemonControl().statusReport() })
        logs = LogsModel(api: api)
        docs = DocsModel(api: api)
        registeredSources = RegisteredSourcesModel(api: api)
        safetyNet = SafetyNetModel(api: api)
        claude = SettingsModel(api: api, providers: ["claude-code", "cc-safety-net", "orch-instructions"])
    }

    /// Starts polling; called once the app has finished launching, and not
    /// at all in a copy that hands off to one already running.
    func start() {
        monitor.start()
        prMonitor.onNewlyNeedingYou = { [notifier] prs in notifier.post(prs) }
        prMonitor.start()
        reviews.onNewVerdicts = { [notifier] reviews in notifier.post(verdicts: reviews) }
        reviews.onNewlyHeld = { [notifier] reviews in notifier.post(held: reviews) }
        reviews.start()
        // A click on a notification opens that PR in the app, not the browser.
        notifier.onOpenPR = { [weak self] url in
            guard let self else { return }
            navigator.showPR(url, in: prs)
            showWindow?()
        }
        notifier.onOpenNeedsYou = { [weak self] in
            guard let self else { return }
            navigator.showNeedsAction(in: prs)
            showWindow?()
        }
        system.startSampling()
        // Docs, Safety Net and Claude change rarely; every few minutes is
        // plenty for a badge, and each tab reloads itself when opened.
        let badges = PeriodicRefresh(interval: .seconds(300), actions: [
            { [docs] in await docs.load() },
            { [safetyNet] in await safetyNet.load() },
            { [claude] in await claude.load() },
        ])
        badges.start()
        self.badges = badges
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
private struct UnreachableDaemon: DaemonAPI, MemoryAPI, ControlAPI, TasksAPI, GitHubAPI, PRReviewAPI, SystemAPI, CatalogAPI,
    SafetyNetAPI, SettingsAPI {
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
    func retry(id: String) async throws -> String { throw failure }
    func cancel(id: String) async throws -> String { throw failure }
    func watchTasks(status: TaskStatusFilter, limit: Int, onEvent: @escaping @Sendable (TaskEvent) async -> Void) async throws {
        throw failure
    }
    func dispatch(_ draft: DispatchDraft) async throws -> DispatchOutcome { throw failure }
    func recommend(taskKind: String) async throws -> (route: RouteInfo?, fromTable: Bool) { throw failure }
    func gitHubIdentity(refresh: Bool) async throws -> GitHubIdentity { throw failure }
    func gitHubScopes(refresh: Bool) async throws -> GitHubScopeList { throw failure }
    func setGitHubScope(_ scope: GitHubScope) async throws -> GitHubScope { throw failure }
    func gitHubRepos(owner: String) async throws -> [GitHubRepo] { throw failure }
    func gitHubItems() async throws -> GitHubItemList { throw failure }
    func prReviews() async throws -> [PRReview] { throw failure }
    func watchPRReview(url: String, mode: PRReviewMode, comment: String?, instructions: String?,
                       reviewAnyway: Bool) async throws -> PRReview {
        throw failure
    }
    func stopPRReview(url: String, forget: Bool) async throws { throw failure }
    func prDetail(url: String, refresh: Bool) async throws -> PRDetail { throw failure }
    func tailLogs(after: UInt64, limit: Int) async throws -> LogPage { throw failure }
    func systemMetrics() async throws -> SystemMetrics { throw failure }
    func listDocs() async throws -> DocCatalog { throw failure }
    func syncDocs(owner: String?, force: Bool) async throws -> DocsSyncState { throw failure }
    func docsSync() async throws -> DocsSyncState { throw failure }
    func safetyNet() async throws -> SafetyNetState { throw failure }
    func installSafetyNet(rulesOnly: Bool) async throws -> SafetyNetState { throw failure }
    func settings() async throws -> SettingsListing { throw failure }
    func setSetting(_ ref: SettingRef, enabled: Bool) async throws -> SettingItem { throw failure }
    func addSetting(provider: String, kind: String, fields: [String: String]) async throws -> SettingItem { throw failure }
    func removeSetting(_ ref: SettingRef) async throws { throw failure }
    func document(uri: String) async throws -> RebuiltDocument { throw failure }
    func registeredSources() async throws -> [RegisteredSource] { throw failure }
    func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource { throw failure }
    func removeSource(id: String, purge: Bool) async throws -> PurgeResult { throw failure }
    func refreshSource(id: String?) async throws { throw failure }
}
