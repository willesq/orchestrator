import OrchKit
import SwiftUI

/// The PR's review watch: what orch may post on its own, how far the review has
/// come and what it waits on, the verdict with its draft comment, and the
/// user's call on it.
struct ReviewCard: View {
    let url: String
    let prs: PRsModel
    let ownPR: Bool

    @State private var mayApprove = false
    @State private var mayComment = false
    @State private var updateInPlace = false
    @State private var draft = ""
    @State private var editingDraft = false
    @State private var showReport = false
    @State private var instructions = ""
    @State private var confirming: PRReviewMode?
    @State private var confirmingAnyway = false
    /// The setup "Review when ready" starts with, and a widening change
    /// waiting for the user's go-ahead.
    @State private var setup = PRReviewSetup.standard
    @State private var confirmingSetup: PRReviewSetup?
    /// The draft as it was when the confirmation opened, which is what gets
    /// posted, even if a new verdict replaces the editor's text meanwhile.
    @State private var confirmedDraft = ""

    var body: some View {
        let review = prs.reviews.byURL[url]
        let saving = prs.saving.contains(url)
        Card("Review", accessory: {
            if saving { ProgressView().controlSize(.small) }
            if let review, review.state?.isActive == true {
                Menu {
                    Button("Stop watching") { Task { await prs.stop(url) } }
                    Button("Stop and forget the verdict", role: .destructive) { Task { await prs.stop(url, forget: true) } }
                } label: { Label("Stop watching", systemImage: "stop.circle") }
                    .menuStyle(.borderlessButton)
                    .fixedSize()
                    .help("Stop watching this PR")
            }
        }) {
            if let review, review.state?.isActive == true {
                ReviewProgress(review: review, task: prs.reviewTasks[review.taskID], checkNow: {
                    Task { await prs.checkNow(url) }
                })
                if review.state == .held { held(review) }
                permissions(watching: review)
                ReviewSetupSection(setup: review.setup) { setSetup($0, review) }
                notes(review)
                if review.verdict != nil {
                    verdict(review)
                    decisions(review)
                }
            } else {
                Text("Orch reviews it once it is out of draft with CI green, using the full adversarial review, and tells you the verdict.")
                    .textStyle(.supporting)
                if let review, let state = review.state {
                    Text(verbatim: displaySafe("\(state.label)\(review.note.isEmpty ? "" : ": \(review.note)")")).textStyle(.caption)
                }
                permissions(watching: nil)
                ReviewSetupSection(setup: setup) { setSetup($0, nil) }
                notes(review)
                HStack {
                    Spacer()
                    Button("Review when ready") {
                        Task {
                            await prs.watch(url, mode: PRReviewMode(mayApprove: mayApprove && !ownPR, mayComment: mayComment),
                                            instructions: instructions, updateInPlace: updateInPlace, setup: setup)
                        }
                    }
                    .buttonStyle(.borderedProminent)
                }
                if let review, review.verdict != nil { verdict(review) }
            }
            if let error = prs.actionErrors[url] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red)
            }
        }
        .disabled(saving)
        .onAppear { sync(review) }
        .onChange(of: review?.comment) { _, _ in draft = review?.comment ?? "" }
        .onChange(of: review?.instructions) { _, _ in instructions = review?.instructions ?? "" }
        .onChange(of: review?.updateInPlace) { _, _ in updateInPlace = review?.updateInPlace ?? false }
        .task(id: review?.taskID) {
            // While a review runs, follow its task so the card can say how it goes.
            while !Task.isCancelled, prs.reviews.byURL[url]?.state == .reviewing {
                await prs.refreshReviewTask(url)
                try? await Task.sleep(for: .seconds(10))
            }
        }
        .confirmationDialog("Review this PR anyway?", isPresented: $confirmingAnyway) {
            Button("Review anyway") {
                if let review = prs.reviews.byURL[url] { Task { await prs.watch(url, mode: review.mode, reviewAnyway: true) } }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("The review runs this PR's code on this Mac with your GitHub sign-in. Do it once you have looked at the changes and trust them. A later push is held again.")
        }
        .confirmationDialog("Let the reviewer use this setup?", isPresented: Binding(get: { confirmingSetup != nil },
                                                                                  set: { if !$0 { confirmingSetup = nil } })) {
            if let next = confirmingSetup {
                Button("Allow") { applySetup(next, prs.reviews.byURL[url]) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(confirmingSetup?.warning ?? "")
        }
        .confirmationDialog(confirmTitle, isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } })) {
            if let choice = confirming {
                Button(confirmButton(choice)) {
                    Task { await prs.watch(url, mode: choice, comment: confirmedDraft) }
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(confirming?.explanation ?? "")
        }
    }

    private func sync(_ review: PRReview?) {
        draft = review?.comment ?? ""
        instructions = review?.instructions ?? ""
        updateInPlace = review?.updateInPlace ?? false
        setup = review?.setup ?? .standard
    }

    /// Changes how the PR's reviews are set up; letting unreviewed content
    /// steer or run in the review asks first, as widening what orch posts does.
    private func setSetup(_ next: PRReviewSetup, _ review: PRReview?) {
        if next.widens(from: review?.setup ?? setup) {
            confirmingSetup = next
        } else {
            applySetup(next, review)
        }
    }

    private func applySetup(_ next: PRReviewSetup, _ review: PRReview?) {
        guard let review, review.state?.isActive == true else {
            setup = next
            return
        }
        Task { await prs.watch(url, mode: review.mode, setup: next) }
    }

    private func confirm(_ mode: PRReviewMode) {
        confirmedDraft = draft
        confirming = mode
    }

    private var confirmTitle: String {
        switch confirming {
        case .comment: "Post this comment on GitHub now?"
        case .verdict, nil: "Stop posting on its own?"
        default: "Let orch post on GitHub for this PR?"
        }
    }

    private func confirmButton(_ mode: PRReviewMode) -> String {
        mode == .comment ? "Post the comment" : "Allow"
    }

    /// What orch may post on its own. Not watched, these set what "Review when
    /// ready" starts with. Watched, they show what orchd holds, never a local
    /// copy, so the card cannot say less than orchd may post; a change asks
    /// first when it widens what is posted.
    @ViewBuilder private func permissions(watching review: PRReview?) -> some View {
        let current = review.map(\.mode) ?? PRReviewMode(mayApprove: mayApprove && !ownPR, mayComment: mayComment)
        VStack(alignment: .leading, spacing: 4) {
            Text("When the verdict is in, orch may:").textStyle(.caption)
            Toggle(isOn: Binding(get: { current.mayApprove && !ownPR },
                                 set: { set(approve: $0, comment: current.mayComment, review) })) {
                Text("Approve the PR, with its comment if it has one")
            }
            .disabled(ownPR)
            .help(ownPR ? "GitHub does not let you approve your own PR" : "Approves only a commit a review said approve to")
            Toggle(isOn: Binding(get: { current.mayComment }, set: { set(approve: current.mayApprove, comment: $0, review) })) {
                Text("Comment its suggestions when it does not approve")
            }
            Toggle(isOn: Binding(get: { review?.updateInPlace ?? updateInPlace }, set: { setUpdateInPlace($0, review) })) {
                Text("Update its earlier comment instead of adding another")
            }
            .help("Also applies to Post comment. An approving verdict updates the comment too, when orch may comment")
            Text(verbatim: "Orch will: \(current.explanation)").textStyle(.caption)
        }
        .toggleStyle(.checkbox)
    }

    private func set(approve: Bool, comment: Bool, _ review: PRReview?) {
        let mode = PRReviewMode(mayApprove: approve && !ownPR, mayComment: comment)
        guard let review else {
            mayApprove = approve
            mayComment = comment
            return
        }
        guard mode != review.mode else { return }
        // Widening what orch posts asks first; narrowing it never needs to.
        let widens = (mode.mayApprove && !review.mode.mayApprove) || (mode.mayComment && !review.mode.mayComment)
        if widens {
            confirm(mode)
        } else {
            Task { await prs.watch(url, mode: mode) }
        }
    }

    private func setUpdateInPlace(_ on: Bool, _ review: PRReview?) {
        guard review != nil else {
            updateInPlace = on
            return
        }
        Task { await prs.setUpdateInPlace(url, on: on) }
    }

    /// Why an untrusted author's PR waits, and the user's go-ahead.
    @ViewBuilder private func held(_ review: PRReview) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "hand.raised").foregroundStyle(.orange)
            Text("The author is not a member or collaborator of this repo. The review runs the PR's code with your GitHub sign-in, so orch waits until you have looked at it.")
                .textStyle(.supporting)
            Spacer()
            Button("Review anyway…") { confirmingAnyway = true }
                .buttonStyle(.borderedProminent)
        }
    }

    /// The user's own notes for this PR's reviews, added to the review prompt.
    @ViewBuilder private func notes(_ review: PRReview?) -> some View {
        let active = review?.state?.isActive == true
        let changed = instructions != (review?.instructions ?? "")
        VStack(alignment: .leading, spacing: 4) {
            Text("Notes for the reviewer: only the reviewing agent and you see them; they are never posted").textStyle(.caption)
            TextField("Optional: what to focus on, context, or what to ignore", text: $instructions, axis: .vertical)
                .lineLimit(2...6)
                .textFieldStyle(.roundedBorder)
                .accessibilityLabel("Notes for the reviewer")
            if active, changed, let review {
                HStack {
                    Text(review.verdict != nil ? "Saving reviews the current commit again with these notes." :
                        "The next review uses these notes.").textStyle(.caption)
                    Spacer()
                    Button("Revert") { instructions = review.instructions }
                    Button("Save notes") { Task { await prs.watch(url, mode: review.mode, instructions: instructions) } }
                        .disabled(instructions.utf8.count > 4000)
                }
                .controlSize(.small)
            }
            if instructions.utf8.count > 4000 {
                Text("Notes are limited to 4000 bytes.").font(.caption).foregroundStyle(.red)
            }
        }
    }

    @ViewBuilder private func verdict(_ review: PRReview) -> some View {
        if let verdict = review.verdict {
            Divider()
            HStack(spacing: 8) {
                Chip(text: verdict.label, tone: ReviewBadge.tone(verdict), symbol: verdict.symbolName)
                    .font(.body.weight(.semibold))
                if review.verdictIsStale { Chip(text: "for an older commit", tone: .warning) }
                Text(verbatim: "on \(PRReview.short(review.headSHA))").textStyle(.caption)
                if let at = review.verdictAt {
                    Text(at, format: .relative(presentation: .named)).textStyle(.caption)
                }
                Spacer()
                if let posted = review.postedLabel {
                    Chip(text: posted, tone: review.posted == "failed" ? .bad : review.posted == "superseded" ? .warning : .good)
                }
            }
            Text(verbatim: displaySafeLines(review.reason))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if !review.forUser.isEmpty || !review.instructions.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    Label("For you", systemImage: "person.crop.circle").font(.headline)
                    if !review.instructions.isEmpty {
                        Text(verbatim: "Your notes: \(displaySafe(review.instructions))")
                            .textStyle(.caption)
                            .lineLimit(3)
                    }
                    if review.forUser.isEmpty {
                        Text("The review did not answer your notes. Its full text is below.").textStyle(.caption)
                    } else {
                        MarkdownBlock(source: review.forUser, base: githubItemURL(url), maxHeight: 280)
                    }
                    Text("Never posted on the PR.").textStyle(.caption)
                }
                .padding(10)
                .background(Color.accentColor.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
            }
            if !review.report.isEmpty {
                DisclosureGroup(isExpanded: $showReport) {
                    MarkdownBlock(source: review.report, base: githubItemURL(url), maxHeight: 480)
                } label: {
                    Text("Full review (for you; never posted)").textStyle(.section)
                }
            }
            HStack {
                Text("Draft comment").textStyle(.section)
                Spacer()
                Picker("Draft", selection: $editingDraft) {
                    Text("Preview").tag(false)
                    Text("Edit").tag(true)
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
            .padding(.top, 4)
            if editingDraft {
                TextEditor(text: $draft)
                    .font(.callout.monospaced())
                    .frame(minHeight: 110, maxHeight: 260)
                    .scrollContentBackground(.hidden)
                    .padding(6)
                    .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
                    .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.surfaceStroke))
                    .accessibilityLabel("Draft review comment")
            } else if draft.isEmpty {
                Text("No comment drafted.").textStyle(.caption)
            } else {
                MarkdownBlock(source: draft, base: githubItemURL(url))
                    .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.surfaceStroke))
            }
        }
    }

    /// The user's own call on this verdict.
    @ViewBuilder private func decisions(_ review: PRReview) -> some View {
        let edited = draft != review.comment
        HStack(spacing: 8) {
            Button("Post comment") { confirm(.comment) }
                .buttonStyle(.borderedProminent)
                .disabled(draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                .help("Orch posts the draft as a review comment on GitHub, pinned to the reviewed commit")
            Spacer()
            if edited {
                Button("Save draft") { Task { await prs.watch(url, mode: review.mode, comment: draft) } }
                Button("Revert") { draft = review.comment }
            }
        }
    }
}

/// How far the review has come, when orch last looked at the PR and looks
/// next, what it is waiting on, and how a review under way is going.
private struct ReviewProgress: View {
    let review: PRReview
    let task: TaskRecord?
    let checkNow: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 4) {
                ForEach(PRReviewStage.allCases, id: \.self) { stage in
                    step(stage)
                    if stage != .posted {
                        Rectangle()
                            .fill(stage < review.stage ? Color.accentColor : Color.surfaceStroke)
                            .frame(height: 2)
                            .frame(maxWidth: 28)
                    }
                }
            }
            .accessibilityElement(children: .combine)
            .accessibilityLabel("Review progress: \(review.stage.label)")
            if !review.note.isEmpty {
                Label { Text(verbatim: displaySafe(review.note)) } icon: { Image(systemName: "info.circle") }
                    .font(.caption)
                    .foregroundStyle(review.state == .failed || review.posted == "failed" ? Color.orange : Color.secondaryText)
            }
            if review.state == .reviewing {
                reviewing
            } else {
                // Ticks every second so the countdown stays true.
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    HStack(spacing: 6) {
                        Text(verbatim: schedule(now: context.date)).textStyle(.caption)
                        Spacer()
                        Button("Check now", action: checkNow)
                            .controlSize(.small)
                            .help("Read the PR from GitHub now")
                    }
                }
            }
        }
    }

    private func step(_ stage: PRReviewStage) -> some View {
        let current = stage == review.stage
        let done = stage < review.stage
        let failed = current && review.state == .failed
        return HStack(spacing: 4) {
            Image(systemName: failed ? "xmark.circle.fill" : done ? "checkmark.circle.fill" : current ? "circle.inset.filled" : "circle")
                .foregroundStyle(failed ? Color.orange : done || current ? Color.accentColor : Color.secondaryText)
            Text(stage.label)
                .font(.caption.weight(current ? .semibold : .regular))
                .foregroundStyle(current ? Color.primary : Color.secondaryText)
                .fixedSize()
        }
    }

    /// "Checked 40 s ago · next check in 1 min" from what orchd reported.
    private func schedule(now: Date) -> String {
        var parts: [String] = []
        if let checked = review.checkedAt {
            parts.append("Checked \(Self.ago(now.timeIntervalSince(checked)))")
        } else {
            parts.append("Not checked yet")
        }
        if let next = review.nextCheckAt {
            let wait = next.timeIntervalSince(now)
            parts.append(wait <= 0 ? "checking now" : "next check in \(Self.span(wait))")
        }
        return parts.joined(separator: " · ")
    }

    @ViewBuilder private var reviewing: some View {
        let run = task?.run
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(verbatim: "Reviewing \(PRReview.short(review.pendingSHA))").textStyle(.supporting)
            if let run {
                Chip(text: run.state.capitalized, tone: run.state == "stalled" ? .warning : .accent)
            }
        }
        if let run {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                VStack(alignment: .leading, spacing: 2) {
                    if let started = run.startedAt {
                        Text(verbatim: "Running for \(Self.span(context.date.timeIntervalSince(started)))").textStyle(.caption)
                    }
                    if let active = run.lastActivity {
                        Text(verbatim: "Last activity \(Self.ago(context.date.timeIntervalSince(active)))").textStyle(.caption)
                    }
                    if run.state == "stalled" {
                        Text("Nothing is running this review. Retry or cancel it in Tasks.").font(.caption).foregroundStyle(.orange)
                    }
                }
            }
        }
        Text(verbatim: "Task \(review.taskID)").textStyle(.codeCaption).textSelection(.enabled)
    }

    static func span(_ seconds: TimeInterval) -> String {
        let s = max(0, Int(seconds.rounded()))
        if s < 60 { return "\(s) s" }
        if s < 3600 { return "\(s / 60) min \(s % 60) s" }
        return "\(s / 3600) h \(s % 3600 / 60) min"
    }

    static func ago(_ seconds: TimeInterval) -> String {
        seconds < 5 ? "just now" : "\(span(seconds)) ago"
    }
}
