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
                enforcement: delegate.model.enforcement,
                openMainWindow: { delegate.showMainWindow() },
                openSpaces: {
                    delegate.model.navigator.showSettings(.spaces)
                    delegate.showMainWindow()
                }
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
    /// orchd, connected once. A feature added since SP-081 reaches its API
    /// through an accessor on it in the feature's own file (AppClient.swift).
    let client: AppClient
    /// The models of features added since SP-081, each built the first time
    /// it is used, so a feature needs no stored property or init line here.
    let features = FeatureStore()
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let notifier = Notifier()
    let refusals = RefusalMonitor()
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
    let handoff: PRHandoffModel
    let landings: PRLandingModel
    let navigator = Navigator()
    let windowControl = WindowControl()
    /// Brings the main window forward; set by the app delegate.
    var showWindow: (@MainActor () -> Void)?
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel
    let claude: SettingsModel
    let config: ConfigModel
    let docsOwners: DocsOwnersModel
    let enforcement: EnforcementModel
    let placement: PlacementValidator
    let spaces: SpacesOverviewModel
    let relabel: SpacesRelabelModel
    /// Keeps the sidebar's badges current between visits to their tabs.
    private(set) var badges: PeriodicRefresh?

    init() {
        let client = AppClient.connect()
        self.client = client
        let api = client.core
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
        handoff = PRHandoffModel(api: api)
        landings = PRLandingModel(api: api)
        system = SystemModel(api: api, loadReport: { try await DaemonControl().statusReport() })
        logs = LogsModel(api: api)
        docs = DocsModel(api: api)
        registeredSources = RegisteredSourcesModel(api: api)
        safetyNet = SafetyNetModel(api: api)
        claude = SettingsModel(api: api, providers: ["claude-code", "cc-safety-net", "orch-instructions"])
        config = ConfigModel(api: api, restartDaemon: { [control] in try await control.perform(.restart) })
        enforcement = EnforcementModel(api: api)
        placement = PlacementValidator(api: api)
        // The Overview and the sidebar badge show a relabel the pane started (SP-079).
        let relabel = SpacesRelabelModel(api: api)
        let spaces = SpacesOverviewModel(api: api, relabel: { await relabel.progress })
        relabel.onChange = { await spaces.load() }
        self.relabel = relabel
        self.spaces = spaces
        docsOwners = DocsOwnersModel(api: api, knownOwners: { [api] in try await api.gitHubScopes(refresh: false).scopes.map(\.owner) })
    }

    /// Starts polling; called once the app has finished launching, and not
    /// at all in a copy that hands off to one already running.
    func start() {
        monitor.start()
        // Notifications name every space's PRs, so none is posted while the
        // Spaces pane's screen-share-safe mode is on.
        prMonitor.onNewlyNeedingYou = { [notifier, spaces] prs in
            guard spaces.allowsNotifications else { return }
            notifier.post(prs)
        }
        prMonitor.start()
        reviews.onNewVerdicts = { [notifier, spaces] reviews in
            guard spaces.allowsNotifications else { return }
            notifier.post(verdicts: reviews)
        }
        reviews.onNewlyHeld = { [notifier, spaces] reviews in
            guard spaces.allowsNotifications else { return }
            notifier.post(held: reviews)
        }
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
        // Each tool call the spaces guard refuses is announced (SP-048).
        refusals.onNewRefusals = { [notifier] found in notifier.post(refusals: found) }
        refusals.start()
        notifier.onOpenRefusals = { [weak self] in
            guard let self else { return }
            navigator.pane = .settings
            navigator.settingsSection = .spaces
            showWindow?()
        }
        system.startSampling()
        // The "enforcement off" banner must show without opening anything.
        enforcement.start()
        // Docs, Safety Net and Claude change rarely; every few minutes is
        // plenty for a badge, and each tab reloads itself when opened.
        let badges = PeriodicRefresh(interval: .seconds(300), actions: [
            { [docs] in await docs.load() },
            { [safetyNet] in await safetyNet.load() },
            { [claude] in await claude.load() },
            { [config] in await config.load() },
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
