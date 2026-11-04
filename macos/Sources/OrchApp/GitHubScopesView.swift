import OrchKit
import SwiftUI

/// Which accounts and orgs orch watches on GitHub, shown in Settings.
struct ScopesColumn: View {
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
    @State private var pickingEverything = false

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
                    if scope.items == .involving {
                        Button("Everything in…") { pickingEverything = true }
                            .controlSize(.small)
                    }
                    Button("Repos…") { pickingRepos = true }
                        .controlSize(.small)
                }
            }
        }
        .padding(.vertical, 2)
        .disabled(model.saving.contains(scope.owner))
        .sheet(isPresented: $pickingRepos) { RepoPicker(scope: scope, model: model) }
        .sheet(isPresented: $pickingEverything) { EverythingRepoPicker(scope: scope, model: model) }
    }
}

private struct RepoPicker: View {
    let scope: GitHubScope
    let model: GitHubModel

    @Environment(\.dismiss) private var dismiss
    @State private var mode: GitHubRepoMode = .all
    @State private var picked: Set<String> = []

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(verbatim: "Repos in \(scope.owner)").font(.title2.bold())
            Picker("Watch", selection: $mode) {
                ForEach(GitHubRepoMode.allCases) { Text($0.label).tag($0) }
            }
            .pickerStyle(.radioGroup)
            if mode != .all {
                RepoChecklist(owner: scope.owner, model: model, picked: $picked)
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

/// Everything open in the repos picked here, on top of what involves the user.
private struct EverythingRepoPicker: View {
    let scope: GitHubScope
    let model: GitHubModel

    @Environment(\.dismiss) private var dismiss
    @State private var picked: Set<String> = []

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(verbatim: "Everything in \(scope.owner) repos").font(.title2.bold())
            Text("Every open PR and issue in these repos, on top of what involves you here.").textStyle(.supporting)
            RepoChecklist(owner: scope.owner, model: model, picked: $picked)
            Text("\(picked.count) selected").font(.caption).foregroundStyle(Color.secondaryText)
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") {
                    Task {
                        // Stay open when it did not save; the error shows in the tab.
                        if await model.setEverythingRepos(scope.owner, repos: Array(picked)) { dismiss() }
                    }
                }
                .keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 460)
        .task {
            picked = Set(scope.everythingRepos)
            await model.loadRepos(scope.owner)
        }
    }
}

/// A filterable list of the owner's repos, each with a checkbox.
private struct RepoChecklist: View {
    let owner: String
    let model: GitHubModel
    @Binding var picked: Set<String>

    @State private var search = ""

    var body: some View {
        TextField("Filter repos", text: $search).textFieldStyle(.roundedBorder)
        if let repos = model.reposFor(owner) {
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
            if let error = model.repoError(owner) {
                HStack {
                    Text(verbatim: "Could not update this list: \(error)").font(.caption).foregroundStyle(.red)
                    Button("Retry") { Task { await model.loadRepos(owner) } }
                }
            }
        } else if let error = model.repoError(owner) {
            VStack(spacing: 8) {
                Text(verbatim: "Could not load repos: \(error)").foregroundStyle(.red)
                Button("Retry") { Task { await model.loadRepos(owner) } }
            }
            .frame(maxWidth: .infinity, minHeight: 260)
        } else {
            ProgressView("Loading repos…").frame(maxWidth: .infinity, minHeight: 260)
        }
    }
}
