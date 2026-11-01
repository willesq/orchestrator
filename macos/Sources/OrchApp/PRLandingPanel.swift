import OrchKit
import SwiftUI

/// What one agent run did, live, read from its session's transcript. Risky
/// commands stay in view however long the run gets.
struct AgentActivityFeed: View {
    let task: TaskRecord

    @Environment(PRLandingModel.self) private var landings
    @State private var expanded: Bool

    init(task: TaskRecord) {
        self.task = task
        _expanded = State(initialValue: task.isUnsettled)
    }

    var body: some View {
        let log = landings.activity[task.id]
        VStack(alignment: .leading, spacing: 4) {
            DisclosureGroup(isExpanded: $expanded) {
                content(log).padding(.top, 4)
            } label: {
                Text(verbatim: summary(log)).textStyle(.caption)
            }
            if let log, log.pushed, PRHandoff.askedNotToPush(task) {
                Label("It ran git push, though its brief said not to. Check the PR's branch.",
                      systemImage: "exclamationmark.triangle.fill")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.red)
            }
        }
        .task(id: "\(task.id)|\(task.isUnsettled)") {
            await landings.followActivity(task.id, running: task.isUnsettled)
        }
    }

    private func summary(_ log: TaskActivityLog?) -> String {
        guard let log, log.available else { return "Activity" }
        let risky = log.risky.count
        let steps = "\(log.total) step\(log.total == 1 ? "" : "s")"
        return risky == 0 ? "\(steps), nothing risky" : "\(steps), \(risky) risky"
    }

    @ViewBuilder
    private func content(_ log: TaskActivityLog?) -> some View {
        if let error = landings.activityErrors[task.id], log == nil {
            VStack(alignment: .leading, spacing: 4) {
                Text(verbatim: "Could not read the session: \(displaySafe(error))").font(.caption).foregroundStyle(.red)
                Button("Try again") { Task { await landings.refreshActivity(task.id) } }.controlSize(.small)
            }
        } else if let log {
            if !log.available {
                Text("No session to read yet: the agent has not started, or it runs on Codex, whose steps orch cannot read.")
                    .textStyle(.caption)
            } else if log.steps.isEmpty {
                Text("The session has started, and has run no tools yet.").textStyle(.caption)
            } else {
                steps(log)
            }
        } else {
            ProgressView().controlSize(.small)
        }
    }

    private func steps(_ log: TaskActivityLog) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 3) {
                    ForEach(log.steps) { StepRow(step: $0).id($0.id) }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxHeight: 170)
            .onChange(of: log.steps.count) { _, _ in
                if task.isUnsettled, let last = log.steps.last { proxy.scrollTo(last.id, anchor: .bottom) }
            }
        }
    }
}

private struct StepRow: View {
    let step: TaskActivityStep

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Image(systemName: step.symbolName)
                .imageScale(.small)
                .frame(width: 14)
                .foregroundStyle(step.risk.isEmpty ? Color.secondaryText : .red)
            if !step.risk.isEmpty {
                Chip(text: step.risk, tone: .bad)
            }
            Text(verbatim: displaySafe(step.text))
                .textStyle(.codeCaption)
                .lineLimit(2)
                .truncationMode(.middle)
                .textSelection(.enabled)
        }
        .help(step.at?.formatted(date: .abbreviated, time: .standard) ?? "")
    }
}

/// The commit a run proposed instead of pushing it: the real change, and the
/// user's choice of what happens to it. Nothing reaches GitHub without Push.
struct PRLandingPanel: View {
    let task: TaskRecord
    let detail: PRDetail?

    @Environment(PRLandingModel.self) private var landings
    @State private var showDiff = false
    @State private var confirmPush = false
    @State private var confirmUndo = false
    @State private var sendingBack = false

    var body: some View {
        let landing = landings.landings[task.id]
        VStack(alignment: .leading, spacing: 8) {
            if let landing {
                switch landing.state {
                case .proposed: proposed(landing)
                case .stale, .invalid: unusable(landing)
                case .onPR: onPR(landing)
                case .landed: landed(landing)
                case .undone: undone(landing)
                case .discarded: discarded(landing)
                case .none: EmptyView()
                }
            } else if landings.errors[task.id] == nil {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text("Checking the agent's commit against the PR's head…").textStyle(.caption)
                }
            }
            if let error = landings.errors[task.id] {
                VStack(alignment: .leading, spacing: 4) {
                    Text(verbatim: displaySafe(error)).font(.caption).foregroundStyle(.red).textSelection(.enabled)
                    Button("Check again") { Task { await landings.load(task.id) } }.controlSize(.small)
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(tint(landing).opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(tint(landing).opacity(0.35)))
        // Reloads as the PR's head moves, so a stale commit says so before
        // anyone reaches for Push.
        .task(id: "\(task.id)|\(detail?.headSHA ?? "")") { await landings.load(task.id) }
        .sheet(isPresented: $showDiff) {
            if let landing { PRLandingDiffSheet(landing: landing) }
        }
        .sheet(isPresented: $sendingBack) {
            if let landing, let detail { SendBackSheet(task: task, landing: landing, detail: detail) }
        }
        .confirmationDialog(pushTitle(landing), isPresented: $confirmPush, presenting: landing) { landing in
            Button("Push \(PRReview.short(landing.sha))") { Task { await landings.land(landing) } }
            Button("Not yet", role: .cancel) {}
        } message: { landing in
            Text(verbatim: "\(landing.commitCount), +\(landing.additions) −\(landing.deletions). \(landing.unseen) Fast-forward only, and refused if the branch has moved. Undo stays available until someone pushes on top.")
        }
        .confirmationDialog("Undo this landing?", isPresented: $confirmUndo, presenting: landing) { landing in
            Button("Push a commit that undoes it") { Task { await landings.undo(landing.taskID) } }
            Button("Keep it", role: .cancel) {}
        } message: { landing in
            Text(verbatim: "Adds one commit to \(displaySafe(landing.branch)) restoring the tree of \(PRReview.short(landing.baseSHA)). History is not rewritten.")
        }
    }

    private func pushTitle(_ landing: PRLanding?) -> String {
        "Push to \(displaySafe(landing?.branch ?? "the PR's branch"))?"
    }

    private func tint(_ landing: PRLanding?) -> Color {
        switch landing?.state {
        case .proposed: .accentColor
        case .landed: .green
        case .stale, .invalid: .orange
        default: .secondary
        }
    }

    private var busy: Bool { landings.acting != nil }

    private func header(_ title: String, symbol: String, tone: Tone) -> some View {
        Label {
            Text(verbatim: title).font(.callout.weight(.semibold))
        } icon: {
            Image(systemName: symbol).foregroundStyle(tone.color)
        }
    }

    @ViewBuilder
    private func proposed(_ landing: PRLanding) -> some View {
        header("Ready to land, nothing pushed yet", symbol: "tray.and.arrow.up.fill", tone: .accent)
        changeSummary(landing)
        ForEach(landing.commits.prefix(4)) { commit in
            HStack(spacing: 6) {
                Text(verbatim: PRReview.short(commit.sha)).textStyle(.codeCaption)
                Text(verbatim: displaySafe(commit.subject)).font(.caption).lineLimit(1)
            }
        }
        if landing.totalCommits > 4 {
            Text(verbatim: "and \(landing.totalCommits - 4) more, in the diff").textStyle(.caption)
        }
        if !landing.blocked.isEmpty {
            Text(verbatim: displaySafe(landing.blocked)).font(.caption).foregroundStyle(.orange)
        }
        if !landing.unseen.isEmpty {
            Text(verbatim: landing.unseen).font(.caption).foregroundStyle(.orange)
        }
        FlowLayout(spacing: 6) {
            Button {
                landings.markReviewed(landing)
                showDiff = true
            } label: { Label("Review diff", systemImage: "doc.text.magnifyingglass") }
            Button { confirmPush = true } label: {
                Label("Push to \(displaySafe(landing.branch))…", systemImage: "arrow.up.circle.fill")
            }
            .buttonStyle(.borderedProminent)
            .disabled(!landings.canPush(landing))
            .help(landings.reviewed.contains(landing.sha) ? "Push this commit to the PR's branch" : "Review the diff first")
            Button("Send back…") { sendingBack = true }.disabled(detail == nil || busy)
            Button("Discard") { Task { await landings.discard(task.id) } }.disabled(busy)
            if landings.acting == task.id { ProgressView().controlSize(.small) }
        }
        .controlSize(.small)
    }

    private func changeSummary(_ landing: PRLanding) -> some View {
        HStack(spacing: 6) {
            Text(verbatim: "\(landing.commitCount) on \(PRReview.short(landing.baseSHA)),")
            Text(verbatim: "+\(landing.additions)").foregroundStyle(.green)
            Text(verbatim: "−\(landing.deletions)").foregroundStyle(.red)
            Text(verbatim: "in \(landing.totalFiles) file\(landing.totalFiles == 1 ? "" : "s")")
        }
        .font(.caption.monospacedDigit())
    }

    @ViewBuilder
    private func unusable(_ landing: PRLanding) -> some View {
        header(landing.state == .stale ? "The PR moved on" : "This commit cannot land",
               symbol: "exclamationmark.triangle.fill", tone: .warning)
        Text(verbatim: displaySafe(landing.problem)).font(.caption).textSelection(.enabled)
        HStack(spacing: 6) {
            Button("Send back…") { sendingBack = true }.disabled(detail == nil || busy)
            Button("Discard") { Task { await landings.discard(task.id) } }.disabled(busy)
        }
        .controlSize(.small)
    }

    @ViewBuilder
    private func onPR(_ landing: PRLanding) -> some View {
        header("Already on the PR", symbol: "exclamationmark.circle.fill", tone: .warning)
        Text(verbatim: "\(PRReview.short(landing.sha)) is the PR's head, so it reached GitHub some other way than Push here. Check the activity above.")
            .font(.caption)
    }

    @ViewBuilder
    private func landed(_ landing: PRLanding) -> some View {
        header("Pushed \(PRReview.short(landing.sha)) to \(displaySafe(landing.branch))", symbol: "checkmark.circle.fill", tone: .good)
        changeSummary(landing)
        if !landing.blocked.isEmpty {
            Text(verbatim: displaySafe(landing.blocked)).textStyle(.caption)
        }
        HStack(spacing: 6) {
            Button { showDiff = true } label: { Label("Review diff", systemImage: "doc.text.magnifyingglass") }
                .disabled(landing.diff.isEmpty)
            Button { confirmUndo = true } label: { Label("Undo…", systemImage: "arrow.uturn.backward") }
                .disabled(!landing.canUndo || busy)
            if landings.acting == task.id { ProgressView().controlSize(.small) }
        }
        .controlSize(.small)
    }

    @ViewBuilder
    private func undone(_ landing: PRLanding) -> some View {
        header("Undone", symbol: "arrow.uturn.backward.circle", tone: .neutral)
        Text(verbatim: "\(PRReview.short(landing.revertSHA)) on \(displaySafe(landing.branch)) restored the tree of \(PRReview.short(landing.baseSHA)). \(PRReview.short(landing.sha)) stays in its history.")
            .font(.caption)
    }

    @ViewBuilder
    private func discarded(_ landing: PRLanding) -> some View {
        header("Discarded, nothing pushed", symbol: "tray", tone: .neutral)
        Text(verbatim: "The agent's worktree stays at \(displaySafe(landing.path)), for you to remove when you are done with it.")
            .font(.caption)
            .textSelection(.enabled)
        Button("Restore") { Task { await landings.restore(task.id) } }.controlSize(.small).disabled(busy)
    }
}

/// The real diff the commit would bring to the PR, verbatim.
private struct PRLandingDiffSheet: View {
    let landing: PRLanding

    @Environment(\.dismiss) private var dismiss
    @State private var lines: [PRDiffLine] = []

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: "\(PRReview.short(landing.baseSHA)) → \(PRReview.short(landing.sha))").font(.title3.bold())
                Text(verbatim: "\(landing.commitCount), +\(landing.additions) −\(landing.deletions), onto \(displaySafe(landing.branch))")
                    .textStyle(.caption)
            }
            if !landing.unseen.isEmpty {
                Text(verbatim: "\(landing.unseen) Send it back for a smaller commit if you cannot check the rest.")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            ScrollView([.vertical, .horizontal]) {
                LazyVStack(alignment: .leading, spacing: 0) {
                    ForEach(lines) { DiffLineRow(line: $0) }
                }
                .padding(6)
                .textSelection(.enabled)
            }
            .overlay(RoundedRectangle(cornerRadius: 4).stroke(.quaternary))
            HStack {
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
            }
        }
        .padding(20)
        .frame(minWidth: 760, idealWidth: 860, minHeight: 520, idealHeight: 640)
        .task { lines = PRDiffLine.parse(landing.diff) }
    }
}

private struct DiffLineRow: View {
    let line: PRDiffLine

    var body: some View {
        Text(verbatim: displaySafe(line.text).isEmpty ? " " : displaySafe(line.text))
            .font(.caption.monospaced().weight(line.kind == .file ? .semibold : .regular))
            .foregroundStyle(color)
            .fixedSize()
            .padding(.top, line.kind == .file ? 10 : 0)
    }

    private var color: Color {
        switch line.kind {
        case .added: .green
        case .removed: .red
        case .hunk: .accentColor
        case .meta: .secondaryText
        case .file, .context: .primary
        }
    }
}

/// Sends a proposal back as a new local-only run, with the user's feedback, and
/// shows the exact brief first.
private struct SendBackSheet: View {
    let task: TaskRecord
    let landing: PRLanding
    let detail: PRDetail

    @Environment(PRHandoffModel.self) private var handoff
    @Environment(PRLandingModel.self) private var landings
    @Environment(\.dismiss) private var dismiss
    @State private var feedback = ""
    @State private var sending = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Send back to the agent").font(.title2.bold())
            Text("A new run gets your feedback and proposes a new commit. It cannot push, and this proposal is discarded once the run is queued.")
                .textStyle(.caption)
            TextField("What should change?", text: $feedback, axis: .vertical)
                .lineLimit(3...6)
                .textFieldStyle(.roundedBorder)
            Text("Brief the agent gets").textStyle(.section)
            ScrollView {
                Text(verbatim: displaySafeLines(brief))
                    .font(.callout.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(6)
            }
            .frame(minHeight: 180, maxHeight: 260)
            .overlay(RoundedRectangle(cornerRadius: 4).stroke(.quaternary))
            if let problem = handoff.dispatch.lastError {
                Text(verbatim: displaySafe(problem)).font(.callout).foregroundStyle(.red)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Send back ⌘↩") { Task { await send() } }
                    .keyboardShortcut(.return, modifiers: .command)
                    .buttonStyle(.borderedProminent)
                    .disabled(sending || feedback.allSatisfy(\.isWhitespace))
            }
        }
        .padding(20)
        .frame(width: 600)
        .onDisappear { handoff.dispatch.reset() }
    }

    private var brief: String {
        PRHandoff.sendBackBrief(detail: detail, landing: landing, feedback: feedback)
    }

    private func send() async {
        guard !sending else { return }
        sending = true
        defer { sending = false }
        var draft = PRHandoffDraft(goal: .other)
        draft.editedBrief = brief
        draft.agent = task.agent
        if await handoff.handOff(draft, detail: detail, review: nil, ownPR: true) {
            await landings.discard(task.id)
            dismiss()
        }
    }
}

/// The chip a PR row or card shows while a proposal waits on the user.
struct ReadyToLandChip: View {
    var body: some View {
        Chip(text: "Ready to land", tone: .accent, symbol: "tray.and.arrow.up.fill",
             help: "An agent proposed a commit for this PR. Nothing is pushed until you choose to.")
    }
}
