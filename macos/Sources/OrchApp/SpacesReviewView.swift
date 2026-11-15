import OrchKit
import SwiftUI

/// Review (SP-054): the newest draft against the applied revision, line by
/// line, with orchd's validation on the lines it refuses, the effect
/// counts, drift, and Apply behind Touch ID or the login password.
struct SpacesReviewView: View {
    let model: SpacesReviewModel
    /// Called after an apply, so the Overview reads the new policy.
    var onApplied: () -> Void = {}

    var body: some View {
        Group {
            if let review = model.review, let diff = model.diff {
                ScrollView {
                    VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
                        status(diff, review)
                        messages
                        if review.reviewedRevision == 0 {
                            EmptyState(symbol: "checkmark.seal", title: "Nothing to review",
                                       message: diff.appliedRevision > 0
                                           ? "No draft is newer than the applied revision (rev \(diff.appliedRevision)). Save one with orch spaces draft."
                                           : "No spaces policy is saved. Save a draft with orch spaces draft, then review it here.")
                        } else {
                            ReviewLines(review: review)
                            if !review.unplaced.isEmpty { UnplacedProblems(problems: review.unplaced) }
                            EffectsCard(effects: review.effects)
                            confirmations(diff)
                            actions(review)
                        }
                    }
                    .padding(Metrics.pagePadding)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
            } else if let error = model.lastError {
                EmptyState(symbol: "exclamationmark.triangle", title: "The draft could not be read", message: error,
                           actionTitle: "Try Again", action: { Task { await model.load() } })
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task { await model.load() }
    }

    @ViewBuilder private func status(_ diff: SpacesPolicyDiff, _ review: SpacesReview) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(verbatim: diff.appliedRevision > 0 ? "Applied policy · rev \(diff.appliedRevision)" : "No policy applied")
                .textStyle(.supporting)
            if review.reviewedRevision > 0 {
                Banner(symbol: "arrow.triangle.branch", tone: .warning,
                       text: "Draft rev \(review.reviewedRevision)\(saved(diff)) is pending. Nothing in it is enforced until you apply it.")
            }
            ForEach(model.drift.filter { $0 != "draft" }, id: \.self) { kind in
                Banner(symbol: "exclamationmark.triangle.fill", tone: .bad, text: SpacesOverview.driftText(kind) + ".")
            }
        }
    }

    private func saved(_ diff: SpacesPolicyDiff) -> String {
        var text = ""
        if let at = diff.draftCreatedAt {
            text += ", saved " + at.formatted(.relative(presentation: .named))
        }
        text += diff.draftByOperator ? " by you" : " by an agent session"
        if !diff.draftByOperator && !diff.draftCreatedBy.isEmpty { text += " (\(displaySafe(diff.draftCreatedBy)))" }
        return text
    }

    @ViewBuilder private var messages: some View {
        if let notice = model.notice {
            Label { Text(verbatim: notice) } icon: { Image(systemName: "checkmark.circle") }.textStyle(.supporting)
        }
        if let error = model.lastError {
            Label { Text(verbatim: error) } icon: { Image(systemName: "exclamationmark.triangle") }
                .foregroundStyle(Tone.bad.color).font(.callout)
        }
    }

    @ViewBuilder private func confirmations(_ diff: SpacesPolicyDiff) -> some View {
        if model.needsAgentAcceptance || !diff.unconfirmedGitHubOrgs.isEmpty {
            Card("Before you apply") {
                if model.needsAgentAcceptance {
                    Toggle("I reviewed this draft, which an agent session saved",
                           isOn: Binding(get: { model.acceptAgentDraft }, set: { model.acceptAgentDraft = $0 }))
                }
                ForEach(diff.unconfirmedGitHubOrgs, id: \.self) { org in
                    Toggle(isOn: Binding(get: { model.confirmOrgs.contains(org) }, set: { on in
                        if on { model.confirmOrgs.insert(org) } else { model.confirmOrgs.remove(org) }
                    })) {
                        Text(verbatim: "Confirm GitHub org \(displaySafe(org))'s current identity")
                    }
                    .help("orchd saw a different GitHub node ID for this org; its new content stays in quarantine until you confirm it")
                }
            }
        }
    }

    private func actions(_ review: SpacesReview) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 10) {
                if review.excludableCount > 0 {
                    Button(review.excludableCount == 1 ? "Drop 1 Invalid Line" : "Drop \(review.excludableCount) Invalid Lines") {
                        Task { await model.dropInvalidLines() }
                    }
                    .disabled(!model.canDrop)
                    .help("Save the draft without the invalid lines as a new revision, for you to review and apply")
                }
                Button("Apply rev \(review.reviewedRevision)") {
                    Task {
                        await model.apply()
                        onApplied()
                    }
                }
                .buttonStyle(.borderedProminent)
                .disabled(!model.canApply)
                .help("Asks for Touch ID or your password, then applies exactly this revision")
                Button("Refresh") { Task { await model.load() } }.disabled(model.isWorking)
            }
            Text(footnote(review)).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func footnote(_ review: SpacesReview) -> String {
        var parts: [String] = []
        if model.needsAgentAcceptance && !model.acceptAgentDraft {
            parts.append("An agent session saved this draft: tick \"I reviewed this draft\" before dropping lines or applying.")
        }
        if review.hasProblems {
            parts.append(review.excludableCount > 0 && review.excludableCount == review.invalidLines.count && review.unplaced.isEmpty
                ? "orchd refuses to apply a draft with problems, so dropping saves a new revision without them; you review and apply that one."
                : "orchd refuses to apply a draft with problems. Fix the marked lines in the draft (orch spaces draft); a line the applied policy already has is not dropped for you.")
        }
        parts.append("Apply asks for Touch ID or your password before it calls orchd, and names the revision shown: if a newer draft was saved meanwhile, orchd refuses it.")
        return parts.joined(separator: " ")
    }
}

/// A one-line notice with a symbol.
private struct Banner: View {
    let symbol: String
    let tone: Tone
    let text: String

    var body: some View {
        Label { Text(verbatim: text).fixedSize(horizontal: false, vertical: true) } icon: {
            Image(systemName: symbol).foregroundStyle(tone.color)
        }
        .font(.callout)
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(tone.color.opacity(0.10), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

/// The draft line by line: + added, − removed, invalid lines marked with
/// orchd's rule and message.
private struct ReviewLines: View {
    let review: SpacesReview

    var body: some View {
        Card("Changes") {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(review.lines) { line in
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(alignment: .firstTextBaseline, spacing: 6) {
                            Text(marker(line)).foregroundStyle(color(line)).frame(width: 12, alignment: .center)
                                .accessibilityLabel(line.change == .same ? "" : line.change.rawValue)
                            Text(verbatim: line.text)
                                .strikethrough(line.invalid && line.excludable)
                                .foregroundStyle(line.change == .same ? Color.secondaryText : Color.primary)
                                .fontWeight(line.nested ? .regular : .semibold)
                                .padding(.leading, line.nested ? 16 : 0)
                                .lineLimit(2).truncationMode(.middle)
                        }
                        .font(.callout.monospaced())
                        // Below the line, so a narrow column keeps both readable.
                        ForEach(line.problems, id: \.self) { p in
                            VStack(alignment: .leading, spacing: 2) {
                                Chip(text: displaySafe(p.rule), tone: .bad, symbol: "xmark.octagon.fill")
                                Text(verbatim: displaySafe(p.message) + (line.excludable ? "" : " Fix this in the draft."))
                                    .textStyle(.caption)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                            .padding(.leading, 34)
                        }
                    }
                    .padding(.vertical, 2)
                    .padding(.horizontal, 4)
                    .background(line.invalid ? Tone.bad.color.opacity(0.08) : Color.clear,
                                in: RoundedRectangle(cornerRadius: 4, style: .continuous))
                    .accessibilityElement(children: .combine)
                }
            }
        }
    }

    private func marker(_ line: ReviewLine) -> String {
        switch line.change {
        case .added: "+"
        case .removed: "−"
        case .same: " "
        }
    }

    private func color(_ line: ReviewLine) -> Color {
        switch line.change {
        case .added: .green
        case .removed: .red
        case .same: .secondaryText
        }
    }
}

private struct UnplacedProblems: View {
    let problems: [SpacesProblemInfo]

    var body: some View {
        Card("Other problems") {
            ForEach(problems, id: \.self) { p in
                VStack(alignment: .leading, spacing: 2) {
                    Chip(text: displaySafe(p.rule), tone: .bad, symbol: "xmark.octagon.fill")
                    Text(verbatim: displaySafe(p.message)).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }
}

private struct EffectsCard: View {
    let effects: [ReviewEffect]

    var body: some View {
        Card("Effect of applying") {
            Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                ForEach(effects) { e in
                    GridRow {
                        Text(e.label).textStyle(.supporting)
                        Text("\(e.count)").monospacedDigit().gridColumnAlignment(.trailing)
                    }
                }
            }
            Text("Live sessions keep their space: a pin never changes. Counts of chunks, PRs and tasks that would move are not reported by orchd yet.")
                .textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
    }
}
