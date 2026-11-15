import AppKit
import OrchKit
import SwiftUI

/// The basic setup wizard (SP-052), shown as a sheet over the Spaces pane:
/// Scan, Spaces, Recall, Guard and Review. Nothing that is enforced changes
/// until Apply, which asks for Touch ID or the login password.
struct SpacesSetupView: View {
    let model: SpacesSetupModel
    /// Closes the sheet with a note for the pane, or nil.
    let finish: (String?) -> Void

    var body: some View {
        VStack(spacing: 0) {
            SetupStepper(current: model.step, done: model.outcome != nil)
                .padding(.horizontal, Metrics.pagePadding)
                .padding(.vertical, 12)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: Metrics.stackSpacing) { content }
                    .padding(Metrics.pagePadding)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            Divider()
            footer.padding(.horizontal, Metrics.pagePadding).padding(.vertical, 12)
        }
        .frame(minWidth: 620, idealWidth: 760, minHeight: 520, idealHeight: 640)
        .task { if model.scan == nil && model.loadError == nil && !model.scanning { await model.load() } }
    }

    @ViewBuilder private var content: some View {
        if let outcome = model.outcome {
            SetupOutcomeView(outcome: outcome)
        } else {
            switch model.step {
            case .scan: SetupScanStep(model: model)
            case .spaces: SetupSpacesStep(model: model)
            case .recall: SetupRecallStep()
            case .enforcement: SetupGuardStep(model: model)
            case .review: SetupReviewStep(model: model)
            }
        }
    }

    @ViewBuilder private var footer: some View {
        HStack {
            if model.outcome != nil {
                Spacer()
                Button("Done") { finish(nil) }.keyboardShortcut(.defaultAction)
            } else {
                Button("Cancel") { Task { finish(await model.cancel()) } }
                    .keyboardShortcut(.cancelAction)
                    .disabled(model.applying)
                    .help("Close setup; nothing it proposed is applied")
                Spacer()
                if model.step != .scan {
                    Button("Back") { model.back() }.disabled(model.applying)
                }
                if model.step == .review {
                    Button("Apply…") { Task { await model.apply() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(!model.canApply)
                        .help("Asks for Touch ID or your password, then applies exactly what this page shows")
                } else {
                    Button("Continue") { Task { await model.next() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(!model.canContinue)
                }
            }
        }
    }
}

/// The five steps, the current one highlighted.
private struct SetupStepper: View {
    let current: SpacesSetupModel.Step
    let done: Bool

    var body: some View {
        FlowLayout(spacing: 14, lineSpacing: 6) {
            ForEach(SpacesSetupModel.Step.allCases) { step in
                HStack(spacing: 5) {
                    Text("\(step.rawValue + 1)")
                        .font(.caption.weight(.bold))
                        .frame(width: 18, height: 18)
                        .background(fill(step), in: Circle())
                        .foregroundStyle(step <= current || done ? Color.white : Color.secondaryText)
                    Text(step.title).font(.callout.weight(step == current && !done ? .semibold : .regular))
                        .foregroundStyle(step == current && !done ? Color.primary : Color.secondaryText)
                }
                .accessibilityElement(children: .combine)
                .accessibilityAddTraits(step == current ? .isSelected : [])
            }
        }
    }

    private func fill(_ step: SpacesSetupModel.Step) -> Color {
        if done || step < current { return .green }
        return step == current ? .accentColor : Color.secondaryText.opacity(0.2)
    }
}

/// Step 1: which folders to scan, and what the scan found.
private struct SetupScanStep: View {
    let model: SpacesSetupModel

    var body: some View {
        Text("orch looks at what is already on this Mac and in your GitHub accounts. Nothing is indexed or changed during setup.")
            .textStyle(.supporting)
        if let error = model.loadError {
            SetupError(text: "The applied policy could not be read: " + error)
        }
        Card("Folders", accessory: {
            HStack {
                Button("Add Folder…", action: addFolder)
                Button("Scan Again") { Task { await model.runScan() } }.disabled(model.scanning || model.folders.isEmpty)
            }
        }) {
            if model.folders.isEmpty {
                Text("No folder chosen. Add the folders that hold your git checkouts.").textStyle(.supporting)
            }
            ForEach(model.folders, id: \.self) { folder in
                HStack {
                    Text(verbatim: SpacesOverview.abbreviate(folder, home: model.home)).textStyle(.code)
                    Spacer()
                    Button { model.removeFolder(folder) } label: { Image(systemName: "minus.circle") }
                        .buttonStyle(.borderless)
                        .help("Do not scan this folder")
                        .accessibilityLabel("Remove \(folder)")
                }
            }
        }
        if model.scanning {
            HStack(spacing: 8) { ProgressView().controlSize(.small); Text("Scanning…").textStyle(.supporting) }
        } else if let error = model.scanError {
            SetupError(text: "The scan failed: " + error)
        }
        if let scan = model.scan {
            ScanFindings(scan: scan, groups: model.groups, home: model.home)
        }
    }

    private func addFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = true
        panel.directoryURL = URL(fileURLWithPath: model.home)
        panel.prompt = "Scan"
        if panel.runModal() == .OK { for url in panel.urls { model.addFolder(url.path) } }
    }
}

private struct ScanFindings: View {
    let scan: SpacesScan
    let groups: [SetupGroup]
    let home: String

    var body: some View {
        let withoutRemote = scan.checkouts.filter { !$0.hasRemote }.count
        Card("Git checkouts") {
            Text(verbatim: summary(withoutRemote: withoutRemote)).textStyle(.supporting)
            if scan.truncated {
                Label("The scan stopped at a limit; some checkouts may be missing.", systemImage: "exclamationmark.triangle")
                    .textStyle(.caption)
            }
            ForEach(scan.problems, id: \.self) { p in
                Text(verbatim: displaySafe(p)).textStyle(.caption)
            }
        }
        Card("GitHub organizations") {
            if scan.accounts.isEmpty {
                Text("No gh login found. Log in with gh auth login, then scan again.").textStyle(.supporting)
            }
            ForEach(Array(scan.accounts.enumerated()), id: \.offset) { _, a in
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: (a.profile.isEmpty ? "Default login" : "Profile of " + displaySafe(a.profile))
                        + (a.login.isEmpty ? "" : " · " + displaySafe(a.login))).textStyle(.rowTitle)
                    Text(verbatim: a.error.isEmpty ? (a.orgs.isEmpty ? "no organizations" : a.orgs.map(displaySafe).joined(separator: ", "))
                        : displaySafe(a.error)).textStyle(.caption)
                }
            }
        }
        Card("Repo visibility") {
            if groups.isEmpty { Text("No checkout has a GitHub remote.").textStyle(.supporting) }
            ForEach(groups) { g in
                HStack(alignment: .firstTextBaseline) {
                    Text(verbatim: displaySafe(g.owner)).textStyle(.rowTitle)
                    Spacer()
                    Text(verbatim: g.evidence).textStyle(.caption)
                }
            }
        }
        Card("Chat workspaces") {
            Text("No Slack or Teams connector is set up. You can add one later.").textStyle(.supporting)
        }
    }

    private func summary(withoutRemote: Int) -> String {
        let n = scan.checkouts.count
        var text = n == 1 ? "1 checkout found" : "\(n) checkouts found"
        text += ", grouped by GitHub owner into \(groups.count) " + (groups.count == 1 ? "group" : "groups")
        if withoutRemote > 0 { text += "; \(withoutRemote) without a GitHub remote, placed by their folder" }
        return text + "."
    }
}

/// Step 5: what Apply would do, and orchd's own view of the draft.
private struct SetupReviewStep: View {
    let model: SpacesSetupModel

    var body: some View {
        if model.reviewing {
            HStack(spacing: 8) { ProgressView().controlSize(.small); Text("Checking the draft…").textStyle(.supporting) }
        }
        if let error = model.reviewError { SetupError(text: error) }
        if !model.reviewProblems.isEmpty {
            SetupError(text: "orchd refuses this draft. Go back and change the choices it names:")
            ForEach(Array(model.reviewProblems.enumerated()), id: \.offset) { _, p in
                Text(verbatim: "• " + displaySafe(p.message)).textStyle(.caption)
            }
        }
        if !model.hasChanges {
            Text("Nothing to change: the applied policy, the switches and the command log already match these choices.")
                .textStyle(.supporting)
        } else {
            Text("Ready to apply. Here is what will happen:").textStyle(.supporting)
            Card { SetupEffects(model: model) }
        }
        if !model.reviewChanges.isEmpty {
            Card("Policy changes") {
                ForEach(Array(model.reviewChanges.enumerated()), id: \.offset) { _, c in
                    VStack(alignment: .leading, spacing: 1) {
                        Text(verbatim: (c.space.isEmpty ? "Policy" : displaySafe(c.space)) + " · " + displaySafe(c.kind))
                            .textStyle(.rowTitle)
                        ForEach(c.details, id: \.self) { Text(verbatim: displaySafe($0)).textStyle(.codeCaption) }
                    }
                }
            }
        }
        if let note = draftNote { Text(verbatim: note).textStyle(.caption) }
        if let error = model.applyError { SetupError(text: error) }
    }

    private var draftNote: String? {
        guard let draft = model.draft else { return nil }
        var note = "Saved as draft revision \(draft.revision) for review; it changes nothing until you apply."
        if let replaced = model.replacedDraft {
            note += " It replaces the pending draft revision \(replaced.revision) as the newest; that one is not applied."
        }
        return note
    }
}

/// The effect counts on the Review step.
private struct SetupEffects: View {
    let model: SpacesSetupModel

    var body: some View {
        Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 16, verticalSpacing: 8) {
            ForEach(rows, id: \.0) { label, value in
                GridRow {
                    Text(label).textStyle(.supporting).gridColumnAlignment(.leading)
                    Text(verbatim: value).textStyle(.rowTitle).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }

    private var rows: [(String, String)] {
        var rows: [(String, String)] = []
        if let plan = model.plan, plan.changesPolicy {
            let created = plan.newSpaces
            if !created.isEmpty {
                rows.append(("Spaces created", "\(created.count): " + created.map { "\(displaySafe($0.name)) (\($0.kind.shortTitle))" }
                    .joined(separator: ", ")))
            }
            let extended = plan.spaces.filter { !$0.isNew }
            if !extended.isEmpty { rows.append(("Spaces extended", extended.map { displaySafe($0.name) }.joined(separator: ", "))) }
            rows.append(("GitHub orgs bound", "\(plan.spaces.reduce(0) { $0 + $1.orgs.count })"))
            rows.append(("Folder roots bound", "\(plan.spaces.reduce(0) { $0 + $1.roots.count })"))
            let placed = plan.placedCounts.map { "\(displaySafe($0.space)) \($0.count)" }.joined(separator: " · ")
            rows.append(("Checkouts placed (estimate)", placed.isEmpty ? "none" : placed))
            let q = plan.quarantined
            rows.append(("Checkouts to quarantine", q.isEmpty ? "none" : "\(q.count): "
                + q.prefix(5).map { SpacesOverview.abbreviate($0.checkout.path, home: model.home) }.joined(separator: ", ")
                + (q.count > 5 ? "…" : "")))
            if !plan.conflicts.isEmpty { rows.append(("Conflicts (nothing indexed)", "\(plan.conflicts.count)")) }
            // Checkouts no GitHub org places, which a folder root decides.
            let byFolder = plan.placements.filter { !$0.checkout.hasRemote && $0.outcome == "placed" }
            if !byFolder.isEmpty {
                rows.append(("Placed by folder alone", byFolder.prefix(5).map {
                    SpacesOverview.abbreviate($0.checkout.path, home: model.home) + " → " + displaySafe($0.space)
                }.joined(separator: ", ") + (byFolder.count > 5 ? "…" : "")))
            }
            rows.append(("Stored content", "moves into each space's own collection in the background, without re-embedding"))
            let jobs = model.newJobs
            if !jobs.isEmpty {
                let ok = jobs.filter { model.loginStatus(for: $0.space).state == .loggedIn }.count
                rows.append(("Job gh profiles", "\(ok) of \(jobs.count) logged in; a job's orgs sync once its profile is"))
            }
        } else {
            rows.append(("Policy", "unchanged"))
        }
        let changes = model.guardChanges
        rows.append(("Enforcement", changes.isEmpty ? "unchanged" : changes.map {
            "\(displaySafe($0.layer.name).capitalizedFirst) → \(SetupGuardStep.label($0.state))"
        }.joined(separator: ", ")))
        if let r = model.retentionChange {
            rows.append(("Command log", "kept " + CommandLogRetention.label(r) + (model.retention?.needsRestart == true ? " (after orchd restarts)" : "")))
        } else {
            rows.append(("Command log", "unchanged"))
        }
        return rows
    }
}

/// What Apply did.
private struct SetupOutcomeView: View {
    let outcome: SpacesSetupModel.Outcome

    var body: some View {
        Label(outcome.failed.isEmpty ? "Setup applied" : "Setup applied, with problems", systemImage: "checkmark.seal")
            .font(.title3.weight(.semibold))
        ForEach(outcome.done, id: \.self) { Text(verbatim: $0).textStyle(.supporting) }
        ForEach(outcome.failed, id: \.self) { SetupError(text: $0) }
        Text("The Spaces pane shows each space as orchd places content from now on. You can run setup again; it only proposes changes.")
            .textStyle(.caption)
    }
}

/// An error or a refusal, in red.
struct SetupError: View {
    let text: String

    var body: some View {
        Label { Text(verbatim: text) } icon: { Image(systemName: "exclamationmark.triangle.fill") }
            .foregroundStyle(.red)
            .textStyle(.supporting)
            .fixedSize(horizontal: false, vertical: true)
    }
}
