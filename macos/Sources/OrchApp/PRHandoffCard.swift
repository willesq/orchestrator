import OrchKit
import SwiftUI

extension EnvironmentValues {
    /// Shows a task in the Tasks pane.
    @Entry var openTask: (String) -> Void = { _ in }
}

/// The PR's agents: the tasks working on it, live, with Cancel, and the
/// hand-offs its state calls for. Nothing is dispatched without the form.
struct PRHandoffCard: View {
    let url: String
    let detail: PRDetail?
    let review: PRReview?
    /// The user wrote the PR, so its code is theirs to run and push to.
    let ownPR: Bool

    @Environment(PRHandoffModel.self) private var handoff
    @Environment(\.openTask) private var openTask
    @State private var goal: PRHandoffGoal?
    @State private var confirmCancel: TaskRecord?

    var body: some View {
        let linked = handoff.tasks(for: url)
        let suggestions = detail.map { PRHandoff.suggestions(detail: $0, review: review) } ?? []
        Card("Agents", accessory: { menu }) {
            VStack(alignment: .leading, spacing: 10) {
                if linked.isEmpty {
                    Text(suggestions.isEmpty
                         ? "No agent has worked on this PR. Hand it off to have one change its branch."
                         : "This PR is blocked. Hand it off and an agent works on it while you watch it here.")
                        .textStyle(.caption)
                } else {
                    ForEach(linked.prefix(4)) { task in
                        VStack(alignment: .leading, spacing: 6) {
                            HandoffTaskRow(task: task, cancelling: handoff.cancelling == task.id,
                                           open: { openTask(task.id) },
                                           cancel: task.canCancel ? { confirmCancel = task } : nil)
                            AgentActivityFeed(task: task)
                            if PRHandoff.proposes(task) {
                                PRLandingPanel(task: task, detail: detail)
                            }
                        }
                    }
                }
                if !suggestions.isEmpty, let detail {
                    FlowLayout(spacing: 6) {
                        ForEach(suggestions) { goal in
                            Button { start(goal, detail: detail) } label: {
                                Label(goal.label, systemImage: goal.symbolName)
                            }
                            .buttonStyle(.borderedProminent)
                            .tint(linked.contains(where: \.isUnsettled) ? .secondary : .accentColor)
                            .controlSize(.small)
                        }
                    }
                }
                if let error = handoff.lastError {
                    Text(verbatim: displaySafe(error)).font(.caption).foregroundStyle(.red)
                }
            }
        }
        .sheet(item: $goal) { goal in
            if let detail {
                HandoffSheet(goal: goal, detail: detail, review: review, ownPR: ownPR, busy: handoff.working(on: url))
            }
        }
        .confirmationDialog("Cancel this agent?", isPresented: Binding(get: { confirmCancel != nil },
                                                                       set: { if !$0 { confirmCancel = nil } }),
                            presenting: confirmCancel) { task in
            Button("Cancel task", role: .destructive) { Task { await handoff.cancel(task.id) } }
            Button("Keep working", role: .cancel) {}
        } message: { task in
            Text(verbatim: displaySafe(task.headline))
        }
    }

    private var menu: some View {
        Menu {
            ForEach(PRHandoffGoal.allCases) { goal in
                Button { if let detail { start(goal, detail: detail) } } label: {
                    Label(goal.label, systemImage: goal.symbolName)
                }
            }
        } label: {
            Label("Hand off", systemImage: "paperplane")
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
        .disabled(detail == nil)
        .help(detail == nil ? "Waiting for the PR's details" : "Have a background agent work on this PR")
    }

    private func start(_ goal: PRHandoffGoal, detail: PRDetail) {
        self.goal = goal
    }
}

/// One task on the PR: where it stands, and how to stop or open it.
private struct HandoffTaskRow: View {
    let task: TaskRecord
    let cancelling: Bool
    let open: () -> Void
    let cancel: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            AgentPulse(active: task.isUnsettled, tone: tone)
                .padding(.top, 4)
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: displaySafe(task.headline)).font(.callout).lineLimit(2)
                HStack(spacing: 6) {
                    Text(verbatim: "\(task.agent) · \(status) · \(PRHandoff.mode(task))\(spent)")
                    if let started = task.run?.startedAt, task.isUnsettled {
                        Text(started, style: .relative)
                    }
                }
                .textStyle(.caption)
                if let reason = PRHandoff.stopReason(task) {
                    Text(verbatim: displaySafe(reason)).font(.caption).foregroundStyle(.orange).lineLimit(3)
                }
                if !task.prOutcome.isEmpty {
                    Text(verbatim: displaySafe(task.prOutcome)).textStyle(.caption).lineLimit(2)
                }
            }
            Spacer(minLength: 4)
            if cancelling {
                ProgressView().controlSize(.small)
            } else if let cancel {
                Button("Cancel…", role: .destructive, action: cancel).controlSize(.small)
            }
            Button("Open", action: open)
                .controlSize(.small)
                .help("Show this task in Tasks")
        }
    }

    /// What the run cost, so two runs on one PR can be told apart.
    private var spent: String {
        task.costUSD > 0 ? String(format: " · $%.2f", task.costUSD) : ""
    }

    private var status: String {
        if let run = task.run, task.isUnsettled { return run.isStalled ? "stalled" : run.state }
        return task.status.replacingOccurrences(of: "_", with: " ")
    }

    private var tone: Tone {
        if task.run?.isStalled == true { return .warning }
        switch task.status {
        case "complete": return .good
        case "failed": return .bad
        case "pending", "in_progress": return .accent
        default: return .neutral
        }
    }
}

/// A dot that breathes while an agent works, and rests when it is done.
struct AgentPulse: View {
    let active: Bool
    var tone: Tone = .accent

    @State private var on = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        ZStack {
            if active, !reduceMotion {
                Circle()
                    .stroke(tone.color.opacity(on ? 0 : 0.6), lineWidth: 2)
                    .frame(width: on ? 16 : 8, height: on ? 16 : 8)
            }
            Circle().fill(tone.color).frame(width: 8, height: 8)
        }
        .frame(width: 16, height: 16)
        .onAppear {
            guard active, !reduceMotion else { return }
            withAnimation(.easeOut(duration: 1.4).repeatForever(autoreverses: false)) { on = true }
        }
        .accessibilityHidden(true)
    }
}

/// The chip a PR row or card shows while an agent works on it.
struct AgentChip: View {
    let task: TaskRecord

    var body: some View {
        HStack(spacing: 4) {
            AgentPulse(active: true, tone: task.run?.isStalled == true ? .warning : .accent)
                .frame(width: 10, height: 10)
            Text(verbatim: task.run?.isStalled == true ? "Agent stalled" : "Agent working")
        }
        .font(.caption.weight(.medium))
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(Color.accentColor.opacity(0.18), in: Capsule())
        .foregroundStyle(task.run?.isStalled == true ? Color.orange : Color.accentColor)
        .help(displaySafe("\(task.agent): \(task.headline)"))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(task.run?.isStalled == true ? "Agent stalled" : "Agent working")
    }
}

/// The hand-off form: what to ask for, whether the agent may push, and the
/// exact brief it gets, which can be edited before it goes.
private struct HandoffSheet: View {
    let ownPR: Bool
    let busy: TaskRecord?

    @Environment(PRHandoffModel.self) private var handoff
    @Environment(\.openTask) private var openTask
    @Environment(\.dismiss) private var dismiss
    @State private var form: PRHandoffDraft
    @State private var editing = false
    @State private var queued: DispatchOutcome?
    @State private var sending = false
    /// The user accepts that the agent works on code someone else wrote.
    @State private var trustsCode = false
    /// The PR as it was when the form opened: the polls that refresh the pane
    /// must not change the brief the user is reading.
    @State private var detail: PRDetail
    @State private var review: PRReview?

    init(goal: PRHandoffGoal, detail: PRDetail, review: PRReview?, ownPR: Bool, busy: TaskRecord?) {
        self.ownPR = ownPR
        self.busy = busy
        _detail = State(initialValue: detail)
        _review = State(initialValue: review)
        _form = State(initialValue: PRHandoffDraft(goal: goal))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Hand off to an agent").font(.title2.bold())
                Text(verbatim: displaySafe("\(PRHandoff.reference(detail.url)) · \(detail.title)"))
                    .textStyle(.caption).lineLimit(1)
            }
            if let busy {
                Label {
                    Text(verbatim: displaySafe("An agent is already on this PR: \(busy.headline)"))
                } icon: { Image(systemName: "exclamationmark.triangle") }
                    .font(.callout).foregroundStyle(.orange)
            }
            if !ownPR {
                VStack(alignment: .leading, spacing: 4) {
                    Label {
                        Text(verbatim: displaySafe("Someone else (\(detail.author)) wrote this PR. An agent that checks it out works on their code with your sign-in; the brief tells it not to run that code outside a sandbox, and it cannot push."))
                    } icon: { Image(systemName: "exclamationmark.shield") }
                        .font(.callout).foregroundStyle(.orange)
                    Toggle("I have read this PR and trust the agent to work on it", isOn: $trustsCode)
                }
            }
            Form {
                Picker("Goal", selection: $form.goal) {
                    ForEach(PRHandoffGoal.allCases) { Label($0.label, systemImage: $0.symbolName).tag($0) }
                }
                .disabled(editing)
                Toggle(isOn: $form.mayPush) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(verbatim: displaySafe("Let it push to \(PRHandoff.reference(detail.url).split(separator: "#").first ?? ""):\(detail.headRef)"))
                        Text(!pushAllowed
                             ? "Unavailable: only your own PR, on a branch other than its base, main or master."
                             : form.mayPush
                             ? "It commits and pushes to this branch only: no force-push, merge, or other branch."
                             : "Off: it commits locally, and you review the diff and push it from here.")
                            .textStyle(.caption)
                    }
                }
                .disabled(editing || !pushAllowed)
                TextField("Agent", text: $form.agent, prompt: Text("dev"))
                TextField("Model", text: $form.model, prompt: Text("provider/slug (optional; routing picks one)"))
                TextField("Notes", text: $form.notes, prompt: Text("Anything the agent should know"), axis: .vertical)
                    .lineLimit(1...4)
                    .disabled(editing)
            }
            HStack {
                Text("Brief the agent gets").textStyle(.section)
                Spacer()
                if editing {
                    Button("Use generated") {
                        form.editedBrief = nil
                        editing = false
                    }
                    .controlSize(.small)
                } else {
                    Button("Edit") {
                        form.editedBrief = form.brief(detail: detail, review: review, ownPR: ownPR)
                        editing = true
                    }
                    .controlSize(.small)
                }
            }
            Group {
                if editing {
                    TextEditor(text: Binding(get: { form.editedBrief ?? "" }, set: { form.editedBrief = $0 }))
                        .font(.callout.monospaced())
                } else {
                    ScrollView {
                        Text(verbatim: displaySafeLines(form.brief(detail: detail, review: review, ownPR: ownPR)))
                            .font(.callout.monospaced())
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(6)
                    }
                }
            }
            .frame(minHeight: 200, maxHeight: 280)
            .overlay(RoundedRectangle(cornerRadius: 4).stroke(.quaternary))
            if let problem = handoff.dispatch.lastError {
                Text(verbatim: displaySafe(problem)).font(.callout).foregroundStyle(.red)
            }
            if let queued {
                VStack(alignment: .leading, spacing: 2) {
                    Label {
                        Text(verbatim: "Handed off as \(queued.id)").textSelection(.enabled)
                    } icon: { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
                        .font(.callout)
                    if let route = queued.route {
                        Text(verbatim: "Routed to \(route.label)").textStyle(.caption)
                    }
                    if !queued.workerEnabled {
                        Text("No worker is enabled, so it waits until queue.worker.enabled is on.")
                            .font(.caption).foregroundStyle(.orange)
                    }
                }
            }
            HStack {
                if let queued {
                    Button("Show in Tasks") {
                        dismiss()
                        openTask(queued.id)
                    }
                }
                Spacer()
                if form.needsInstructions, queued == nil {
                    Text("Say what to do in Notes.").textStyle(.caption)
                }
                Button(queued == nil ? "Cancel" : "Done") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                if queued == nil {
                    // ⌘↩, not plain Return: Return in a text field must not start an agent.
                    Button("Hand off ⌘↩") { Task { await send() } }
                        .keyboardShortcut(.return, modifiers: .command)
                        .buttonStyle(.borderedProminent)
                        .disabled(sending || !canSend)
                }
            }
        }
        .padding(20)
        .frame(width: 600)
        .onDisappear { handoff.dispatch.reset() }
    }

    private var pushAllowed: Bool { PRHandoff.canPush(detail, ownPR: ownPR) }

    private var canSend: Bool {
        guard ownPR || trustsCode else { return false }
        guard !form.needsInstructions else { return false }
        return DispatchDraft(agent: form.agent, body: form.brief(detail: detail, review: review, ownPR: ownPR)).problem == nil
    }

    private func send() async {
        guard !sending else { return }
        sending = true
        defer { sending = false }
        if await handoff.handOff(form, detail: detail, review: review, ownPR: ownPR) {
            queued = handoff.dispatch.outcome
        }
    }
}
