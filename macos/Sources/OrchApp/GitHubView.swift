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
                AdaptiveSplit(primaryWidth: .init(min: 300, ideal: 340, max: 440), secondaryWidth: .init(min: 360),
                              stacking: .tabs(primary: "Scopes", secondary: "Activity")) {
                    ScopesColumn(model: model)
                } secondary: {
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

private struct ScopesColumn: View {
    let model: GitHubModel

    var body: some View {
        List {
            Section {
                if let login = model.identity?.login {
                    Label {
                        Text(verbatim: "Signed in as \(login)")
                    } icon: {
                        Image(systemName: "person.crop.circle")
                    }
                    .foregroundStyle(Color.secondaryText)
                }
                if let error = model.scopesError {
                    Text(verbatim: "Could not load your accounts: \(error). Retrying.")
                        .font(.caption).foregroundStyle(.red)
                }
                if let warning = model.warning {
                    Text(verbatim: warning).font(.caption).foregroundStyle(.orange)
                }
                if let error = model.lastError {
                    Text(verbatim: error).font(.caption).foregroundStyle(.red)
                }
            }
            Section("Watching") {
                if model.scopes.isEmpty && model.scopesError == nil {
                    Text("No accounts yet. Check that gh is signed in, then refresh.")
                        .textStyle(.supporting)
                }
                ForEach(model.scopes) { scope in
                    ScopeRow(scope: scope, model: model)
                }
            }
            Section("Your teams") {
                let teams = model.identity?.teams ?? []
                if model.identity != nil && teams.isEmpty {
                    Text("You are not on any teams.").textStyle(.supporting)
                }
                ForEach(teams) { team in
                    VStack(alignment: .leading, spacing: 1) {
                        Text(verbatim: team.name.isEmpty ? team.slug : team.name)
                        Text(verbatim: "\(team.org)/\(team.slug)").textStyle(.caption)
                    }
                }
            }
        }
        .listStyle(.inset)
    }
}

private struct ScopeRow: View {
    let scope: GitHubScope
    let model: GitHubModel

    @State private var pickingRepos = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Toggle(isOn: Binding(get: { scope.enabled }, set: { on in Task { await model.setEnabled(scope.owner, on) } })) {
                    HStack(spacing: 6) {
                        Text(verbatim: scope.owner).font(.body.weight(.medium))
                        Chip(text: scope.isUser ? "You" : "Org")
                    }
                }
                .toggleStyle(.switch)
                .controlSize(.small)
                if model.saving.contains(scope.owner) {
                    ProgressView().controlSize(.mini)
                }
            }
            if scope.enabled {
                Picker("Show", selection: Binding(get: { scope.items }, set: { mode in Task { await model.setItems(scope.owner, mode) } })) {
                    ForEach(GitHubItemsMode.allCases) { Text($0.label).tag($0) }
                }
                .controlSize(.small)
                HStack {
                    Text(verbatim: scope.coverage).textStyle(.caption)
                    Spacer()
                    Button("Repos…") { pickingRepos = true }
                        .controlSize(.small)
                }
            }
        }
        .padding(.vertical, 2)
        .disabled(model.saving.contains(scope.owner))
        .sheet(isPresented: $pickingRepos) { RepoPicker(scope: scope, model: model) }
    }
}

private struct RepoPicker: View {
    let scope: GitHubScope
    let model: GitHubModel

    @Environment(\.dismiss) private var dismiss
    @State private var mode: GitHubRepoMode = .all
    @State private var picked: Set<String> = []
    @State private var search = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(verbatim: "Repos in \(scope.owner)").font(.title2.bold())
            Picker("Watch", selection: $mode) {
                ForEach(GitHubRepoMode.allCases) { Text($0.label).tag($0) }
            }
            .pickerStyle(.radioGroup)
            if mode != .all {
                TextField("Filter repos", text: $search).textFieldStyle(.roundedBorder)
                if let repos = model.reposFor(scope.owner) {
                    List(repos.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) }) { repo in
                        Toggle(isOn: Binding(
                            get: { picked.contains(repo.name) },
                            set: { on in if on { picked.insert(repo.name) } else { picked.remove(repo.name) } }
                        )) {
                            HStack {
                                Text(verbatim: repo.name)
                                if repo.isPrivate { Image(systemName: "lock").font(.caption).foregroundStyle(Color.secondaryText) }
                                if repo.isArchived { Text("archived").font(.caption).foregroundStyle(Color.secondaryText) }
                            }
                        }
                        .toggleStyle(.checkbox)
                    }
                    .frame(minHeight: 260)
                } else if let error = model.repoError(scope.owner) {
                    VStack(spacing: 8) {
                        Text(verbatim: "Could not load repos: \(error)").foregroundStyle(.red)
                        Button("Retry") { Task { await model.loadRepos(scope.owner) } }
                    }
                    .frame(maxWidth: .infinity, minHeight: 260)
                } else {
                    ProgressView("Loading repos…").frame(maxWidth: .infinity, minHeight: 260)
                }
                Text(mode == .include ? "\(picked.count) selected" : "\(picked.count) excluded")
                    .font(.caption).foregroundStyle(Color.secondaryText)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") {
                    Task {
                        // Stay open when it did not save; the error shows in the tab.
                        if await model.setRepos(scope.owner, mode: mode, repos: Array(picked)) { dismiss() }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(mode != .all && picked.isEmpty)
            }
        }
        .padding(20)
        .frame(width: 460)
        .task {
            mode = scope.repoMode
            picked = Set(scope.repos)
            await model.loadRepos(scope.owner)
        }
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
