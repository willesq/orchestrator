import Foundation
import Observation
import OrchKit

/// The GitHub tab's two halves.
enum GitHubSection: String, CaseIterable, Identifiable {
    case overview, prs

    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: "Overview"
        case .prs: "PRs"
        }
    }
}

/// The parts of the Settings pane.
enum SettingsSection: String, CaseIterable, Identifiable {
    case tasks, docs, github, memory, claude, safetyNet, app

    var id: String { rawValue }

    var title: String {
        switch self {
        case .tasks: "Tasks"
        case .docs: "Docs"
        case .github: "GitHub"
        case .memory: "Memory"
        case .claude: "Claude"
        case .safetyNet: "Safety Net"
        case .app: "App"
        }
    }

    var symbol: String {
        switch self {
        case .tasks: "checklist"
        case .docs: "doc.text.magnifyingglass"
        case .github: "point.3.connected.trianglepath.dotted"
        case .memory: "brain.head.profile"
        case .claude: "slider.horizontal.3"
        case .safetyNet: "checkmark.shield"
        case .app: "macwindow"
        }
    }

    /// The config.yaml sections this part shows.
    var configSections: [String] {
        switch self {
        case .tasks: ["Tasks", "Task verification", "Codex", "Model routing"]
        case .docs: ["Docs"]
        case .github: ["GitHub"]
        case .memory: ["Sources", "Prompt context", "Session summaries", "Search", "Embedding", "Vector store"]
        case .claude, .safetyNet, .app: []
        }
    }
}

/// Where the main window is: its pane and the GitHub tab's section, both kept
/// across launches. A notification click moves it.
@MainActor
@Observable
final class Navigator {
    var pane: Pane {
        didSet { UserDefaults.standard.set(pane.rawValue, forKey: "pane") }
    }

    var gitHubSection: GitHubSection {
        didSet { UserDefaults.standard.set(gitHubSection.rawValue, forKey: "githubSection") }
    }

    var settingsSection: SettingsSection {
        didSet { UserDefaults.standard.set(settingsSection.rawValue, forKey: "settingsSection") }
    }

    /// Set to open the dispatch sheet the next time the Tasks pane shows;
    /// the pane clears it once the sheet is up.
    var dispatchRequested = false

    init() {
        let defaults = UserDefaults.standard
        let stored = defaults.string(forKey: "pane")
        // Claude and Safety Net were panes of their own before Settings.
        let moved = stored.flatMap(SettingsSection.init(rawValue:)).flatMap { [.claude, .safetyNet].contains($0) ? $0 : nil }
        pane = moved != nil ? .settings : stored.flatMap(Pane.init(rawValue:)) ?? .dashboard
        settingsSection = moved ?? defaults.string(forKey: "settingsSection").flatMap(SettingsSection.init(rawValue:)) ?? .tasks
        gitHubSection = defaults.string(forKey: "githubSection").flatMap(GitHubSection.init(rawValue:)) ?? .overview
    }

    /// Shows one part of the Settings pane.
    func showSettings(_ section: SettingsSection) {
        pane = .settings
        settingsSection = section
    }

    /// Shows one PR in the GitHub tab's PRs pane.
    func showPR(_ url: String, in prs: PRsModel) {
        pane = .github
        gitHubSection = .prs
        prs.select(url)
    }

    /// Shows the PRs pane narrowed to what needs the user.
    func showNeedsAction(in prs: PRsModel) {
        pane = .github
        gitHubSection = .prs
        prs.clearFilters()
        prs.state = .needsAction
    }
}
