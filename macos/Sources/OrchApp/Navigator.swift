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

    init() {
        let defaults = UserDefaults.standard
        pane = defaults.string(forKey: "pane").flatMap(Pane.init(rawValue:)) ?? .dashboard
        gitHubSection = defaults.string(forKey: "githubSection").flatMap(GitHubSection.init(rawValue:)) ?? .overview
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
