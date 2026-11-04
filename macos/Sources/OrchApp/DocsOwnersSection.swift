import OrchKit
import SwiftUI

/// Which GitHub accounts, orgs and repos have their docs synced into memory.
/// Opting out stops the sync and keeps what is indexed, with a separate,
/// confirmed way to delete it.
struct DocsOwnersSection: View {
    let model: DocsOwnersModel

    @State private var newOwner = ""

    var body: some View {
        Section {
            if model.rows.isEmpty {
                Text("No accounts yet. Check that gh is signed in, or add one by name.").textStyle(.supporting)
            }
            ForEach(model.rows) { DocsOwnerRowView(row: $0, model: model) }
            HStack {
                TextField("Add an account or org by name", text: $newOwner)
                    .onSubmit(add)
                Button("Add", action: add).disabled(newOwner.isEmpty)
            }
            if let message = model.message {
                Text(verbatim: message).textStyle(.caption)
            }
            if let error = model.lastError {
                ErrorText(error)
            }
        } header: {
            Text("Synced into memory")
        } footer: {
            Text("Turning one on clones its repos and indexes their markdown. Archived repos and forks follow the settings below.")
                .textStyle(.caption)
        }
    }

    private func add() {
        Task { if await model.add(newOwner) { newOwner = "" } }
    }
}

private struct DocsOwnerRowView: View {
    let row: DocsOwnerRow
    let model: DocsOwnersModel

    @State private var choosingRepos = false
    @State private var removing = false
    @State private var purging = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle(isOn: Binding(get: { row.isOn }, set: { on in Task { await model.setEnabled(row.login, on) } })) {
                HStack(spacing: 8) {
                    Text(verbatim: row.login)
                    if let owner = row.owner {
                        Chip(text: owner.coverage)
                    }
                    if model.pending.contains(row.login) {
                        ProgressView().controlSize(.small).accessibilityLabel("Saving")
                    }
                }
            }
            .disabled(model.pending.contains(row.login))
            if let owner = row.owner {
                if !owner.error.isEmpty {
                    Text(verbatim: displaySafe(owner.error)).font(.caption).foregroundStyle(.orange).lineLimit(3)
                }
                HStack(spacing: 12) {
                    Button("Repos…") { choosingRepos = true }.disabled(!owner.enabled)
                    Button("Remove…") { removing = true }
                    let kept = owner.keptRepos
                    if !kept.isEmpty {
                        Spacer(minLength: 8)
                        Text(kept.count == 1 ? "1 opted-out repo is still indexed" : "\(kept.count) opted-out repos are still indexed")
                            .textStyle(.caption)
                        Button("Delete Its Docs…") { purging = true }
                    }
                }
                .buttonStyle(.link)
                .font(.caption)
                .sheet(isPresented: $choosingRepos) { DocsRepoPicker(owner: owner, model: model) }
                .confirmationDialog("Stop tracking \(row.login)?", isPresented: $removing, titleVisibility: .visible) {
                    Button("Remove, Keep Its Docs") { Task { await model.remove(row.login, purge: false) } }
                    Button("Remove and Delete Its Docs", role: .destructive) { Task { await model.remove(row.login, purge: true) } }
                    Button("Cancel", role: .cancel) {}
                } message: {
                    Text("Deleting its docs is permanent. They can only come back by opting in and syncing again.")
                }
                .confirmationDialog("Delete the docs of opted-out repos?", isPresented: $purging, titleVisibility: .visible,
                                    presenting: owner.keptRepos.map(\.repo)) { repos in
                    Button("Delete Docs", role: .destructive) { Task { await model.purgeKept(row.login, repos: repos) } }
                    Button("Cancel", role: .cancel) {}
                } message: { repos in
                    Text(verbatim: "This permanently removes them from memory: "
                        + repos.prefix(8).map(displaySafe).joined(separator: ", ")
                        + (repos.count > 8 ? ", and \(repos.count - 8) more." : "."))
                }
            }
            if let error = model.ownerErrors[row.login] {
                ErrorText(error)
            }
        }
    }
}

private struct DocsRepoPicker: View {
    let owner: DocsOwner
    let model: DocsOwnersModel

    @Environment(\.dismiss) private var dismiss
    @State private var mode: DocsRepoMode = .all
    @State private var picked: Set<String> = []
    @State private var search = ""
    @State private var saveError: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(verbatim: "Docs from \(owner.owner)").font(.title2.bold())
            Picker("Sync", selection: $mode) {
                ForEach(DocsRepoMode.allCases) { Text($0.label).tag($0) }
            }
            .pickerStyle(.radioGroup)
            if mode != .all {
                TextField("Filter repos", text: $search).textFieldStyle(.roundedBorder)
                checklist
                Text(mode == .include ? "\(picked.count) selected" : "\(picked.count) excluded").textStyle(.caption)
            }
            Text("Repos you opt out keep their indexed docs until you delete them.").textStyle(.caption)
            if let saveError {
                ErrorText(saveError)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") {
                    Task {
                        // Stay open when it did not save, and say why.
                        if await model.setRepos(owner.owner, mode: mode, repos: Array(picked)) {
                            dismiss()
                        } else {
                            saveError = model.ownerErrors[owner.owner]
                        }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(mode != .all && picked.isEmpty)
            }
        }
        .padding(20)
        .frame(width: 460)
        .task {
            mode = owner.repoMode
            picked = Set(owner.selectedRepos)
            await model.loadRepos(owner.owner)
        }
    }

    @ViewBuilder
    private var checklist: some View {
        if let repos = model.repos(owner.owner) {
            List(repos.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) }) { repo in
                Toggle(isOn: Binding(
                    get: { picked.contains(repo.name) },
                    set: { on in if on { picked.insert(repo.name) } else { picked.remove(repo.name) } }
                )) {
                    HStack {
                        Text(verbatim: displaySafe(repo.name))
                        if repo.isArchived { Text("archived").textStyle(.caption) }
                        if repo.isFork { Text("fork").textStyle(.caption) }
                    }
                }
                .toggleStyle(.checkbox)
            }
            .frame(minHeight: 240)
        } else if let error = model.repoError(owner.owner) {
            VStack(spacing: 8) {
                ErrorText("Could not load repos: \(error)", font: .body)
                Button("Retry") { Task { await model.loadRepos(owner.owner) } }
            }
            .frame(maxWidth: .infinity, minHeight: 240)
        } else {
            ProgressView("Loading repos…").frame(maxWidth: .infinity, minHeight: 240)
        }
    }
}
