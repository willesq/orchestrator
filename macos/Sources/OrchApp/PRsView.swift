import AppKit
import OrchKit
import SwiftUI

/// The GitHub tab's PRs pane: PRs from the accounts the user watches, narrowed
/// by role, state and repo, with the selected PR's detail and review beside.
struct PRsView: View {
    let github: GitHubModel
    @Bindable var prs: PRsModel

    var body: some View {
        HSplitView {
            PRListColumn(github: github, prs: prs)
                .frame(minWidth: 320, idealWidth: 380, maxWidth: 520)
            PRDetailView(prs: prs, login: github.identity?.login)
                .frame(minWidth: 420, maxWidth: .infinity, maxHeight: .infinity)
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
            filters(repos: prRepos(all))
            Divider()
            Group {
                if github.items == nil, let error = github.lastError {
                    EmptyState(symbol: "exclamationmark.triangle", title: "PRs unavailable", message: error,
                               actionTitle: "Try again", action: { Task { await github.refreshItems() } })
                } else if github.items == nil {
                    ProgressView()
                } else if visible.isEmpty {
                    empty
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

    private func filters(repos: [String]) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            TextField("Search titles or #number", text: $prs.search)
                .textFieldStyle(.roundedBorder)
            HStack(spacing: 6) {
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

    @ViewBuilder private var empty: some View {
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

/// One PR in the list: title, where it is, and what it needs.
private struct PRRow: View {
    let item: GitHubItem
    let review: PRReview?

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
                }
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 3)
        .contentShape(Rectangle())
        .contextMenu {
            if let url = item.webURL {
                Button("Open on GitHub") { NSWorkspace.shared.open(url) }
                Button("Copy link") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(url.absoluteString, forType: .string)
                }
            }
        }
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
