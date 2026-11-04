import AppKit
import OrchKit
import SwiftUI

struct GitHubView: View {
    @Bindable var model: GitHubModel
    let prs: PRsModel
    @Bindable var navigator: Navigator

    @AppStorage(PRsLayout.storageKey) private var prsLayout = PRsLayout.list

    var body: some View {
        Group {
            switch navigator.gitHubSection {
            case .overview:
                VStack(spacing: 0) {
                    WatchingSummary(model: model) { navigator.showSettings(.github) }
                    Divider()
                    ActivityColumn(model: model)
                }
            case .prs:
                PRsView(github: model, prs: prs)
            }
        }
        .safeAreaInset(edge: .top) {
            if let warning = model.checkWarning {
                Text(verbatim: warning)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(8)
                    .background(.bar)
            }
        }
        .navigationTitle("GitHub")
        .toolbar {
            ToolbarItem(placement: .navigation) {
                Picker("Section", selection: $navigator.gitHubSection) {
                    ForEach(GitHubSection.allCases) { section in
                        Text(sectionLabel(section)).tag(section)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
            if navigator.gitHubSection == .overview {
                ToolbarItem(placement: .principal) {
                    Picker("Show", selection: $model.filter) {
                        ForEach(GitHubModel.Filter.allCases) { Text($0.rawValue).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    .fixedSize()
                }
            }
            if navigator.gitHubSection == .prs {
                ToolbarItem(placement: .principal) {
                    Picker("Layout", selection: $prsLayout) {
                        ForEach(PRsLayout.allCases) { layout in
                            Label(layout.title, systemImage: layout.symbolName).tag(layout)
                        }
                    }
                    .pickerStyle(.segmented)
                    .labelStyle(.titleAndIcon)
                    .labelsHidden()
                    .fixedSize()
                    .help("Show PRs as a list, or as a board of lanes by where each stands")
                }
            }
            ToolbarItem {
                if model.isCheckingGitHub {
                    ProgressView()
                        .controlSize(.small)
                        .accessibilityLabel("Checking GitHub")
                        .help("Checking GitHub")
                } else {
                    Button {
                        Task {
                            await prs.reviews.refresh()
                            await model.checkGitHub(rediscover: navigator.gitHubSection == .overview)
                        }
                    } label: {
                        Label("Refresh", systemImage: "arrow.clockwise")
                    }
                    .disabled(model.isRefreshing)
                    .help(navigator.gitHubSection == .overview ? "Check GitHub for new activity, and for your orgs and teams"
                        : "Check GitHub for new PRs and reload reviews")
                }
            }
        }
        .onAppear { model.start() }
        .onDisappear { model.stop() }
    }

    /// PRs carries how many verdicts wait on the user.
    private func sectionLabel(_ section: GitHubSection) -> String {
        let verdicts = prs.reviews.verdictCount
        return section == .prs && verdicts > 0 ? "PRs (\(verdicts))" : section.title
    }
}

/// What is being watched, in a line. Choosing it lives in Settings.
private struct WatchingSummary: View {
    let model: GitHubModel
    let openSettings: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Text(verbatim: summary).textStyle(.supporting).lineLimit(1)
            Spacer(minLength: 8)
            Button("Change in Settings…", action: openSettings).buttonStyle(.link)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private var summary: String {
        let on = model.scopes.filter(\.enabled).map(\.owner)
        return on.isEmpty ? "Not watching anything on GitHub." : "Watching " + on.joined(separator: ", ")
    }
}

private struct ActivityColumn: View {
    @Bindable var model: GitHubModel

    var body: some View {
        let sections = model.sections
        VStack(spacing: 0) {
            HStack {
                Text("Activity").textStyle(.section)
                Spacer()
                if let updated = model.items?.updatedAt {
                    Text("updated \(updated, format: .relative(presentation: .named))").textStyle(.caption)
                }
            }
            .padding(12)
            Divider()
            Group {
                if model.items == nil, let error = model.lastError {
                    EmptyState(symbol: "exclamationmark.triangle", title: "Activity unavailable", message: error,
                               actionTitle: "Try again", action: { Task { await model.refreshItems() } })
                } else if model.items == nil {
                    ProgressView()
                } else if sections.needsAction.isEmpty && sections.other.isEmpty {
                    empty
                } else {
                    List {
                        if !sections.needsAction.isEmpty {
                            Section {
                                ForEach(sections.needsAction) { ItemRow(item: $0) }
                            } header: {
                                header("Needs action", count: sections.needsAction.count)
                            }
                        }
                        if !sections.other.isEmpty {
                            Section {
                                ForEach(sections.other) { ItemRow(item: $0) }
                            } header: {
                                header("Everything else", count: sections.other.count)
                            }
                        }
                    }
                    .listStyle(.inset)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }

    private func header(_ title: String, count: Int) -> some View {
        HStack {
            Text(title).textStyle(.section)
            Spacer()
            Text("\(count)").textStyle(.caption).monospacedDigit()
        }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isHeader)
    }

    /// Says what the filter hides, and offers to show everything.
    @ViewBuilder private var empty: some View {
        switch model.filter {
        case .all:
            EmptyState(symbol: "checkmark.circle", title: "Nothing open",
                       message: "Open PRs and issues in the accounts you watch show up here.")
        case .prs:
            EmptyState(symbol: "arrow.triangle.pull", title: "No open PRs",
                       message: "None of the accounts you watch has an open PR involving you.",
                       actionTitle: "Show everything", action: { model.filter = .all })
        case .issues:
            EmptyState(symbol: "smallcircle.filled.circle", title: "No open issues",
                       message: "None of the accounts you watch has an open issue involving you.",
                       actionTitle: "Show everything", action: { model.filter = .all })
        }
    }
}

private struct ItemRow: View {
    let item: GitHubItem

    var body: some View {
        // Items untouched for months are quieter, so live work stands out.
        let stale = item.isStale()
        Button {
            if let url = item.webURL { NSWorkspace.shared.open(url) }
        } label: {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: item.isPR ? "arrow.triangle.pull" : "smallcircle.filled.circle")
                    .foregroundStyle(item.attention != nil ? Color.red : stale ? Color.secondaryText : item.isPR ? Color.purple : Color.green)
                    .frame(width: 16)
                    .accessibilityLabel(item.isPR ? "Pull request" : "Issue")
                VStack(alignment: .leading, spacing: 3) {
                    Text(verbatim: displaySafe(item.title))
                        .lineLimit(2)
                        .foregroundStyle(stale ? Color.secondaryText : Color.primary)
                    HStack(spacing: 6) {
                        Text(verbatim: displaySafe("\(item.repo)#\(item.number)"))
                        if !item.author.isEmpty { Text(verbatim: displaySafe("by \(item.author)")) }
                        if let when = item.updatedAt {
                            Text(when, format: .relative(presentation: .named))
                        }
                        if item.isDraft { Text("draft") }
                    }
                    .font(.caption)
                    .foregroundStyle(stale ? Color.tertiaryText : Color.secondaryText)
                    let reasons = item.chipReasons
                    if item.attention != nil || !reasons.isEmpty {
                        HStack(spacing: 4) {
                            if let attention = item.attention {
                                Chip(text: attention.label, tone: .bad, symbol: attention.symbolName)
                            }
                            if !stale {
                                ForEach(reasons, id: \.self) { Chip(text: $0.label) }
                            }
                        }
                    }
                }
                Spacer(minLength: 0)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(item.webURL == nil)
        .padding(.vertical, 3)
        .help(item.webURL?.absoluteString ?? displaySafe(item.url))
    }
}
