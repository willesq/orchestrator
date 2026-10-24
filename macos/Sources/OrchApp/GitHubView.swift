import AppKit
import OrchKit
import SwiftUI

struct GitHubView: View {
    @Bindable var model: GitHubModel

    var body: some View {
        HSplitView {
            ScopesColumn(model: model)
                .frame(minWidth: 300, idealWidth: 340, maxWidth: 440)
            ActivityColumn(model: model)
                .frame(minWidth: 360, maxWidth: .infinity, maxHeight: .infinity)
        }
        .navigationTitle("GitHub")
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("Show", selection: $model.filter) {
                    ForEach(GitHubModel.Filter.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
            ToolbarItem {
                Button { Task { await model.refresh(rediscover: true) } } label: {
                    Label("Refresh", systemImage: "arrow.clockwise")
                }
                .disabled(model.isRefreshing)
                .help("Ask GitHub for your orgs and teams again, and reload activity")
            }
        }
        .onAppear { model.start() }
        .onDisappear { model.stop() }
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
                    .foregroundStyle(.secondary)
                }
                if let warning = model.warning {
                    Text(verbatim: warning).font(.caption).foregroundStyle(.orange)
                }
                if let error = model.lastError {
                    Text(verbatim: error).font(.caption).foregroundStyle(.red)
                }
            }
            Section("Watching") {
                if model.scopes.isEmpty && model.lastError == nil {
                    Text("No accounts yet. Check that gh is signed in, then refresh.")
                        .font(.callout).foregroundStyle(.secondary)
                }
                ForEach(model.scopes) { scope in
                    ScopeRow(scope: scope, model: model)
                }
            }
            Section("Your teams") {
                let teams = model.identity?.teams ?? []
                if model.identity != nil && teams.isEmpty {
                    Text("You are not on any teams.").font(.callout).foregroundStyle(.secondary)
                }
                ForEach(teams) { team in
                    VStack(alignment: .leading, spacing: 1) {
                        Text(verbatim: team.name.isEmpty ? team.slug : team.name)
                        Text(verbatim: "\(team.org)/\(team.slug)").font(.caption).foregroundStyle(.secondary)
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
                        Text(scope.isUser ? "You" : "Org")
                            .font(.caption2)
                            .padding(.horizontal, 5)
                            .background(.quaternary, in: .capsule)
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
                    Text(verbatim: scope.coverage).font(.caption).foregroundStyle(.secondary)
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
                                if repo.isPrivate { Image(systemName: "lock").font(.caption).foregroundStyle(.secondary) }
                                if repo.isArchived { Text("archived").font(.caption).foregroundStyle(.secondary) }
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
                    .font(.caption).foregroundStyle(.secondary)
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
    let model: GitHubModel

    var body: some View {
        let items = model.visibleItems
        VStack(spacing: 0) {
            HStack {
                Text("Activity").font(.headline)
                Spacer()
                if let updated = model.items?.updatedAt {
                    Text("updated \(updated, format: .relative(presentation: .named))")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .padding(12)
            Divider()
            Group {
                if model.items == nil, let error = model.lastError {
                    ContentUnavailableView("Activity unavailable", systemImage: "exclamationmark.triangle",
                                           description: Text(verbatim: error))
                } else if model.items == nil {
                    ProgressView()
                } else if items.isEmpty {
                    ContentUnavailableView("Nothing open", systemImage: "checkmark.circle",
                                           description: Text("Open PRs and issues in the accounts you watch show up here."))
                } else {
                    List(items) { item in ItemRow(item: item) }
                        .listStyle(.inset)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }
}

private struct ItemRow: View {
    let item: GitHubItem

    var body: some View {
        Button {
            if let url = item.webURL { NSWorkspace.shared.open(url) }
        } label: {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: item.isPR ? "arrow.triangle.pull" : "smallcircle.filled.circle")
                    .foregroundStyle(item.attention != nil ? Color.red : item.isPR ? Color.purple : Color.green)
                    .frame(width: 16)
                VStack(alignment: .leading, spacing: 3) {
                    Text(verbatim: item.title).lineLimit(2)
                    HStack(spacing: 6) {
                        Text(verbatim: "\(item.repo)#\(item.number)")
                        if !item.author.isEmpty { Text(verbatim: "by \(item.author)") }
                        if let when = item.updatedAt {
                            Text(when, format: .relative(presentation: .named))
                        }
                        if item.isDraft { Text("draft") }
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    HStack(spacing: 4) {
                        if let attention = item.attention {
                            Chip(text: attention.label, color: .red)
                        }
                        ForEach(item.reasons, id: \.self) { Chip(text: $0.label, color: .secondary) }
                    }
                }
                Spacer(minLength: 0)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(item.webURL == nil)
        .padding(.vertical, 3)
    }
}

private struct Chip: View {
    let text: String
    let color: Color

    var body: some View {
        Text(text)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(color.opacity(0.15), in: .capsule)
            .foregroundStyle(color)
    }
}
