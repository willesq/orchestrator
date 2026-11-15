import OrchKit
import SwiftUI

/// Step 2: a plain-language kind per GitHub owner, the folder roots as a
/// tree, and each new job's gh profile with its login.
struct SetupSpacesStep: View {
    let model: SpacesSetupModel

    var body: some View {
        Text("Each GitHub owner found became a proposed space. Choose how each one should behave:").textStyle(.supporting)
        if model.groups.isEmpty {
            Text("No checkout with a GitHub remote was found. Go back and add the folders that hold your checkouts.")
                .textStyle(.supporting)
        } else {
            Card {
                VStack(alignment: .leading, spacing: 12) {
                    ForEach(model.groups) { g in
                        GroupRow(model: model, group: g)
                        if g.id != model.groups.last?.id { Divider() }
                    }
                }
            }
            Text("Job keeps a space apart from every other job. Personal is yours alone. Open takes only repos GitHub reports public; the rest go to quarantine for you to place. Open spaces get no folders: a local folder becomes public only once it is listed explicitly (D-65).")
                .textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
        if let plan = model.plan, !plan.roots.isEmpty { RootTree(roots: plan.roots, home: model.home) }
        let jobs = model.newJobs
        if !jobs.isEmpty {
            Card("A GitHub account per job") {
                Text("Each job uses its own gh profile (GH_CONFIG_DIR). orch only reads its status; you log in yourself.")
                    .textStyle(.caption)
                ForEach(jobs, id: \.space.name) { job in
                    ProfileRow(model: model, group: job.group, space: job.space)
                }
            }
        }
    }
}

private struct GroupRow: View {
    let model: SpacesSetupModel
    let group: SetupGroup

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline) {
                Text(verbatim: displaySafe(group.owner)).textStyle(.section)
                Text(verbatim: where_ + " · " + group.evidence).textStyle(.caption).lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 0)
            }
            if let bound = group.boundSpace {
                Chip(text: "Already in \(displaySafe(bound))", tone: .neutral, symbol: "lock",
                     help: "In the applied policy; change it in the advanced editor")
            } else {
                Picker("Behaves as", selection: Binding(get: { model.kind(of: group) },
                                                        set: { model.setKind($0, for: group.id) })) {
                    ForEach(SetupKind.allCases) { Text($0 == .job ? $0.title : $0.shortTitle).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
                .accessibilityLabel("How \(displaySafe(group.owner)) behaves")
            }
            if model.kind(of: group) == .open && !group.notPublic.isEmpty {
                let names = group.notPublic.map { displaySafe($0.owner + "/" + $0.repo) }
                Label("\(names.count == 1 ? "1 repo is" : "\(names.count) repos are") not public and \(names.count == 1 ? "goes" : "go") to quarantine: "
                      + names.joined(separator: ", "), systemImage: "tray.full")
                    .textStyle(.caption).foregroundStyle(.orange)
            }
        }
        .accessibilityElement(children: .contain)
    }

    private var where_: String {
        let folders = Set(group.checkouts.map { ($0.path as NSString).deletingLastPathComponent })
        let n = group.checkouts.count
        let count = n == 1 ? "1 checkout" : "\(n) checkouts"
        if folders.count == 1, let f = folders.first { return SpacesOverview.abbreviate(f, home: model.home) + " · " + count }
        return count + " in \(folders.count) folders"
    }
}

/// Every root, nested like CODEOWNERS, with the ones setup will not bind.
private struct RootTree: View {
    let roots: [SetupRoot]
    let home: String

    var body: some View {
        Card("Folders") {
            ForEach(roots) { r in
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(verbatim: (r.depth > 0 ? String(repeating: "   ", count: r.depth - 1) + "└ " : "")
                        + SpacesOverview.abbreviate(r.path, home: home))
                        .textStyle(.code)
                        .foregroundStyle(r.rejected == nil ? Color.primary : Color.secondaryText)
                    Chip(text: "\(displaySafe(r.space)) (\(r.kind))", tone: r.rejected == nil ? tone(r.kind) : .neutral)
                    if r.existing { Text("applied").textStyle(.caption) }
                    Spacer(minLength: 4)
                    if r.rejected == nil && !r.existing {
                        Text(r.checkouts == 1 ? "1 checkout" : "\(r.checkouts) checkouts").textStyle(.caption)
                    }
                }
                if let why = r.rejected {
                    Text(verbatim: "Not bound: " + why).textStyle(.caption).foregroundStyle(.orange)
                        .padding(.leading, CGFloat(r.depth) * 14)
                }
            }
            Text("The deepest folder decides, and a folder inside another may only be stricter: open < personal < job, and inside a job's folder only that job (INV-14). A checkout no folder holds is placed by its GitHub org.")
                .textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func tone(_ kind: String) -> Tone {
        switch kind {
        case "isolated": .warning
        case "private": .accent
        default: .good
        }
    }
}

private struct ProfileRow: View {
    let model: SpacesSetupModel
    let group: SetupGroup
    let space: SetupSpace

    var body: some View {
        let status = model.loginStatus(for: space)
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                Text(verbatim: displaySafe(space.name)).textStyle(.rowTitle).frame(minWidth: 90, alignment: .leading)
                TextField("gh profile folder", text: Binding(get: { model.profiles[group.id] ?? space.ghConfigDir },
                                                            set: { model.setProfile($0, for: group.id) }))
                    .textFieldStyle(.roundedBorder)
                    .font(.callout.monospaced())
                Chip(text: status.text, tone: status.state == .loggedIn ? .good : (status.state == .notLoggedIn ? .warning : .neutral))
            }
            if let command = status.command {
                Text(verbatim: "To log in: " + displaySafe(command)).textStyle(.codeCaption).textSelection(.enabled)
            }
        }
    }
}

/// Step 3: what each kind of content may do. These are the safe defaults;
/// per-kind rules arrive with the chat connectors (SP-060).
struct SetupRecallStep: View {
    static let rows: [(String, [Bool?])] = [
        ("Repo and GitHub docs", [true, true, true, true]),
        ("Session summaries", [true, true, true, true]),
        ("Session transcripts", [true, false, true, true]),
        ("Chat channels", [true, false, true, true]),
        ("Direct messages", [true, false, true, nil]),
    ]

    var body: some View {
        Text("What may each kind of content do? Basic setup changes none of this; these are the safe defaults. Each space reads only itself, open spaces, and personal spaces it is allowed to.")
            .textStyle(.supporting)
        Card {
            Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 8) {
                GridRow {
                    Text("Content").textStyle(.caption)
                    ForEach(["Store it", "Auto-recall in prompts", "Search on request", "Queued tasks"], id: \.self) {
                        Text($0).textStyle(.caption)
                    }
                }
                Divider().gridCellUnsizedAxes(.horizontal)
                ForEach(Self.rows, id: \.0) { name, cells in
                    GridRow {
                        Text(name).textStyle(.rowTitle)
                        ForEach(Array(cells.enumerated()), id: \.offset) { _, allowed in
                            Text(allowed == true ? "✓" : allowed == false ? "—" : "never")
                                .foregroundStyle(allowed == true ? Color.green : Color.secondaryText)
                                .accessibilityLabel(allowed == true ? "yes" : allowed == false ? "not by default" : "never")
                        }
                    }
                }
            }
        }
        Text("Chat content arrives with the Slack and Teams connectors. Direct messages are never recalled into prompts and never given to queued tasks.")
            .textStyle(.caption).fixedSize(horizontal: false, vertical: true)
    }
}

/// Step 4: the enforcement switches and how long the command log is kept.
struct SetupGuardStep: View {
    let model: SpacesSetupModel

    var body: some View {
        Text("Enforcement is on by default. Each layer has its own switch, and only you can change one; later you can switch a layer off for a while from Settings → Spaces.")
            .textStyle(.supporting)
        Card("Layers") {
            if let state = model.enforcement, !state.switches.isEmpty {
                Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 16, verticalSpacing: 10) {
                    ForEach(state.switches) { s in
                        GridRow {
                            Text(verbatim: displaySafe(s.name).capitalizedFirst).textStyle(.rowTitle)
                            Picker(s.name, selection: Binding(get: { model.guardChoices[s.layer] ?? s.state },
                                                              set: { model.setGuard($0, for: s.layer) })) {
                                ForEach(s.states.isEmpty ? [s.state] : s.states, id: \.self) { st in
                                    Text(Self.label(st) + (st == s.defaultState ? " (default)" : "")).tag(st)
                                }
                            }
                            .labelsHidden()
                            .fixedSize()
                        }
                    }
                }
            } else {
                Text("The switches could not be read; setup leaves them as they are.").textStyle(.supporting)
            }
            Text("A session's space is fixed at its first folder. Block other jobs refuses tool calls that reach another job's folders or its GitHub orgs; everything else is logged. Strict is opt-in.")
                .textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
        Card("Keep the command log for") {
            if let r = model.retention {
                Picker("Keep the command log for", selection: Binding(get: { model.retentionChoice }, set: { model.setRetention($0) })) {
                    ForEach(r.options, id: \.self) { Text(CommandLogRetention.label($0)).tag($0) }
                }
                .pickerStyle(.radioGroup)
                .labelsHidden()
                Text("Refused calls are kept 90 days. A session that is still open is never deleted, only cut to its size cap.")
                    .textStyle(.caption)
            } else {
                Text("The setting could not be read; setup leaves it as it is.").textStyle(.supporting)
            }
        }
    }

    /// A switch state for people.
    static func label(_ state: String) -> String {
        switch state {
        case "on": "On"
        case "off": "Off"
        case "log": "Log only"
        case "block-other-jobs": "Block other jobs"
        case "strict": "Strict"
        default: displaySafe(state)
        }
    }
}

extension String {
    /// "Tool guard" from "tool guard".
    var capitalizedFirst: String { prefix(1).uppercased() + dropFirst() }
}
