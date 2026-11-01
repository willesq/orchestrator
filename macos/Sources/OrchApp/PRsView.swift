import AppKit
import OrchKit
import SwiftUI

/// How the PRs pane lays PRs out, kept across launches.
enum PRsLayout: String, CaseIterable, Identifiable {
    case list, board

    static let storageKey = "prsLayout"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .list: "List"
        case .board: "Board"
        }
    }

    var symbolName: String {
        switch self {
        case .list: "list.bullet"
        case .board: "rectangle.split.3x1"
        }
    }
}

/// The GitHub tab's PRs pane: PRs from the accounts the user watches, narrowed
/// by role, state and repo, as a list or a board of lanes, with the selected
/// PR's detail and review beside.
struct PRsView: View {
    let github: GitHubModel
    @Bindable var prs: PRsModel

    @AppStorage(PRsLayout.storageKey) private var layout = PRsLayout.list

    var body: some View {
        switch layout {
        case .list:
            AdaptiveSplit(primaryWidth: .init(min: 320, ideal: 380, max: 520), secondaryWidth: .init(min: 420),
                          stacking: .detail(shown: prs.selection != nil, backTitle: "PRs", back: { prs.select(nil) })) {
                PRListColumn(github: github, prs: prs)
            } secondary: {
                PRDetailView(prs: prs, login: github.identity?.login)
            }
        case .board:
            PRBoardView(github: github, prs: prs)
        }
    }
}

private struct PRListColumn: View {
    let github: GitHubModel
    @Bindable var prs: PRsModel

    var body: some View {
        let all = github.items?.items ?? []
        let visible = prs.visible(all)
        VStack(spacing: 0) {
            PRFilterBar(github: github, prs: prs, repos: prRepos(all))
            Divider()
            Group {
                if github.items == nil, let error = github.lastError {
                    EmptyState(symbol: "exclamationmark.triangle", title: "PRs unavailable", message: error,
                               actionTitle: "Try again", action: { Task { await github.refreshItems() } })
                } else if github.items == nil {
                    ProgressView()
                } else if visible.isEmpty {
                    PRsEmptyState(prs: prs)
                } else {
                    let reviews = prs.reviews.byURL
                    List(visible, selection: Binding(get: { prs.selection }, set: { prs.select($0) })) { item in
                        PRRow(item: item, review: reviews[item.url]).tag(item.url)
                    }
                    .listStyle(.inset)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

/// Search and the role, state and repo filters, shared by the list and the board.
struct PRFilterBar: View {
    let github: GitHubModel
    @Bindable var prs: PRsModel
    let repos: [String]

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            TextField("Search titles or #number", text: $prs.search)
                .textFieldStyle(.roundedBorder)
            FlowLayout(spacing: 6) {
                Picker("Role", selection: $prs.role) {
                    ForEach(PRRoleFilter.allCases) { Text($0.label).tag($0) }
                }
                Picker("State", selection: $prs.state) {
                    ForEach(PRStateFilter.allCases) { Text($0.label).tag($0) }
                }
                Picker("Repo", selection: $prs.repo) {
                    Text("All repos").tag(String?.none)
                    if let repo = prs.repo, !repos.contains(repo) {
                        Text(verbatim: displaySafe(repo)).tag(Optional(repo))
                    }
                    ForEach(repos, id: \.self) { Text(verbatim: displaySafe($0)).tag(Optional($0)) }
                }
            }
            .labelsHidden()
            .pickerStyle(.menu)
            .controlSize(.small)
            HStack {
                if let updated = github.items?.updatedAt {
                    Text("updated \(updated, format: .relative(presentation: .named))").textStyle(.caption)
                }
                Spacer()
                if prs.isFiltered {
                    Button("Clear filters") { prs.clearFilters() }
                        .buttonStyle(.link)
                        .controlSize(.small)
                }
            }
        }
        .padding(12)
    }
}

/// What the PRs pane says when no PR is left to show.
struct PRsEmptyState: View {
    let prs: PRsModel

    var body: some View {
        if prs.isFiltered {
            EmptyState(symbol: "line.3.horizontal.decrease.circle", title: "No PRs match",
                       message: "None of the open PRs in the accounts you watch pass these filters.",
                       actionTitle: "Clear filters", action: { prs.clearFilters() })
        } else {
            EmptyState(symbol: "arrow.triangle.pull", title: "No open PRs",
                       message: "Open PRs in the accounts you watch show up here. Choose accounts in Overview.")
        }
    }
}

/// Opening and copying a PR's link, for its context menu.
struct PRLinkMenu: View {
    let item: GitHubItem

    var body: some View {
        if let url = item.webURL {
            Button("Open on GitHub") { NSWorkspace.shared.open(url) }
            Button("Copy link") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(url.absoluteString, forType: .string)
            }
        }
    }
}

/// One PR in the list: title, where it is, and what it needs.
private struct PRRow: View {
    let item: GitHubItem
    let review: PRReview?

    @Environment(PRHandoffModel.self) private var handoff
    @Environment(PRLandingModel.self) private var landings

    var body: some View {
        let stale = item.isStale()
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: "arrow.triangle.pull")
                .foregroundStyle(item.attention != nil ? Color.red : stale ? Color.secondaryText : Color.purple)
                .frame(width: 16)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                Text(verbatim: displaySafe(item.title))
                    .lineLimit(2)
                    .foregroundStyle(stale ? Color.secondaryText : Color.primary)
                HStack(spacing: 6) {
                    Text(verbatim: displaySafe("\(item.repo)#\(item.number)"))
                    if !item.author.isEmpty { Text(verbatim: displaySafe("by \(item.author)")) }
                    if let when = item.updatedAt { Text(when, format: .relative(presentation: .named)) }
                }
                .font(.caption)
                .foregroundStyle(Color.secondaryText)
                HStack(spacing: 4) {
                    if let attention = item.attention {
                        Chip(text: attention.label, tone: .bad, symbol: attention.symbolName)
                    }
                    if item.isDraft { Chip(text: "Draft") }
                    if let review { ReviewBadge(review: review) }
                    if let task = handoff.working(on: item.url) {
                        AgentChip(task: task)
                    } else if let proposal = landings.awaiting(in: handoff.tasks(for: item.url)) {
                        ReadyToLandChip(task: proposal)
                    }
                }
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 3)
        .contentShape(Rectangle())
        .contextMenu { PRLinkMenu(item: item) }
    }
}

/// A PR's review in one chip: its verdict when there is one for the current
/// head, else where the review stands.
struct ReviewBadge: View {
    let review: PRReview

    var body: some View {
        if review.state == .verdict, let verdict = review.verdict {
            Chip(text: verdict.label, tone: Self.tone(verdict), symbol: verdict.symbolName,
                 help: "Review verdict for \(PRReview.short(review.headSHA))")
        } else if review.state == .held {
            Chip(text: "Held", tone: .warning, symbol: "hand.raised", help: "Waiting for your go-ahead to review")
        } else if let state = review.state, state.isActive {
            Chip(text: state == .reviewing ? "Reviewing" : state == .failed ? "Review failed" : "Watching",
                 tone: state == .failed ? .warning : .accent, symbol: "eye")
        }
    }

    static func tone(_ verdict: PRVerdict) -> Tone {
        switch verdict {
        case .approve: .good
        case .wait: .warning
        case .changes: .bad
        }
    }
}
