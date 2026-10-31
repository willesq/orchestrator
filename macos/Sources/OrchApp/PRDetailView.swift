import AppKit
import OrchKit
import SwiftUI

/// The selected PR: its review watch and verdict first, then what GitHub says
/// about it. Everything from GitHub or the review is other people's text,
/// shown verbatim.
struct PRDetailView: View {
    let prs: PRsModel
    /// The user's GitHub login, to tell their own PRs, which GitHub never lets
    /// them approve.
    let login: String?

    var body: some View {
        if let url = prs.selection {
            ScrollView {
                VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
                    header(url)
                    ReviewCard(url: url, prs: prs, ownPR: isOwn(url))
                    PRHandoffCard(url: url, detail: prs.details[url], review: prs.reviews.byURL[url], ownPR: isOwn(url))
                    if let detail = prs.details[url] {
                        MetadataCard(detail: detail)
                        if !detail.checks.isEmpty { ChecksCard(detail: detail) }
                        if !detail.files.isEmpty { FilesCard(detail: detail) }
                        if !detail.body.isEmpty {
                            Card("Description") {
                                MarkdownBlock(source: detail.body, base: githubItemURL(url))
                            }
                        }
                    } else if let error = prs.detailErrors[url] {
                        Card("Details") {
                            Text(verbatim: "Could not load the PR from GitHub: \(error)").foregroundStyle(.red)
                            Button("Try again") { Task { await prs.loadDetail(url, refresh: true) } }
                        }
                    } else {
                        ProgressView("Loading from GitHub…").frame(maxWidth: .infinity)
                    }
                }
                .padding(Metrics.pagePadding)
            }
            .id(url)
        } else {
            EmptyState(symbol: "arrow.triangle.pull", title: "Select a PR",
                       message: "Choose a PR to see its details and have it reviewed when it is ready.")
        }
    }

    private func isOwn(_ url: String) -> Bool {
        guard let login, let author = prs.details[url]?.author, !author.isEmpty else { return false }
        return login.caseInsensitiveCompare(author) == .orderedSame
    }

    private func header(_ url: String) -> some View {
        let detail = prs.details[url]
        let web = githubItemURL(url)
        return VStack(alignment: .leading, spacing: 6) {
            Text(verbatim: displaySafe(detail?.title ?? url))
                .font(.title2.weight(.semibold))
                .textSelection(.enabled)
            HStack(spacing: 8) {
                Text(verbatim: displaySafe(web.map { "\($0.pathComponents[1])/\($0.pathComponents[2])#\($0.lastPathComponent)" } ?? url))
                    .lineLimit(1)
                if let detail, !detail.author.isEmpty { Text(verbatim: displaySafe("by \(detail.author)")).lineLimit(1) }
                if let detail { StateChip(detail: detail) }
            }
            .font(.callout)
            .foregroundStyle(Color.secondaryText)
            HStack(spacing: 8) {
                if prs.loadingDetail.contains(url) { ProgressView().controlSize(.small) }
                Button { Task { await prs.loadDetail(url, refresh: true) } } label: {
                    Label("Reload", systemImage: "arrow.clockwise")
                }
                .help("Read this PR from GitHub again")
                if let web {
                    Button { NSWorkspace.shared.open(web) } label: {
                        Label("Open on GitHub", systemImage: "arrow.up.forward.square")
                    }
                    .help(web.absoluteString)
                }
            }
            .font(.callout)
            .foregroundStyle(Color.secondaryText)
        }
    }
}

private struct StateChip: View {
    let detail: PRDetail

    var body: some View {
        switch (detail.state, detail.isDraft) {
        case ("MERGED", _): Chip(text: "Merged", tone: .accent)
        case ("CLOSED", _): Chip(text: "Closed", tone: .bad)
        case (_, true): Chip(text: "Draft")
        default: Chip(text: "Open", tone: .good)
        }
    }
}

private struct MetadataCard: View {
    let detail: PRDetail

    var body: some View {
        Card("Details") {
            Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 6) {
                row("Branches", "\(detail.headRef) → \(detail.baseRef)")
                GridRow {
                    Text("Changes").textStyle(.caption)
                    HStack(spacing: 6) {
                        Text(verbatim: "+\(detail.additions)").foregroundStyle(.green)
                        Text(verbatim: "−\(detail.deletions)").foregroundStyle(.red)
                        Text(verbatim: "in \(detail.changedFiles) file\(detail.changedFiles == 1 ? "" : "s")")
                    }
                    .font(.callout.monospacedDigit())
                }
                row("Head", PRReview.short(detail.headSHA))
                row("CI", ciText)
                row("Review", reviewText)
                if !detail.mergeable.isEmpty { row("Mergeable", detail.mergeable.capitalized) }
                if !detail.requestedReviewers.isEmpty { row("Requested", detail.requestedReviewers.joined(separator: ", ")) }
                if !detail.reviews.isEmpty {
                    row("Reviews", detail.reviews.map { "\($0.login): \($0.state.lowercased().replacingOccurrences(of: "_", with: " "))" }
                        .joined(separator: ", "))
                }
                if !detail.labels.isEmpty { row("Labels", detail.labels.joined(separator: ", ")) }
                if let created = detail.createdAt {
                    GridRow {
                        Text("Opened").textStyle(.caption)
                        Text(created, format: .relative(presentation: .named)).font(.callout)
                    }
                }
            }
        }
    }

    private var ciText: String {
        let s = detail.checkSummary
        switch detail.ciState {
        case "none": return "No checks"
        case "success": return "Passing (\(s.passing))"
        case "failure": return "Failing (\(s.failing) of \(detail.checks.count))"
        case "pending": return "Running (\(s.running) of \(detail.checks.count))"
        default: return detail.ciState
        }
    }

    private var reviewText: String {
        switch detail.reviewDecision {
        case "APPROVED": "Approved"
        case "CHANGES_REQUESTED": "Changes requested"
        case "REVIEW_REQUIRED": "Review required"
        default: "No review decision"
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).textStyle(.caption)
            Text(verbatim: displaySafe(value)).font(.callout).textSelection(.enabled)
        }
    }
}

private struct ChecksCard: View {
    let detail: PRDetail
    @State private var expanded = false

    var body: some View {
        Card {
            DisclosureGroup(isExpanded: $expanded) {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(Array(detail.checks.enumerated()), id: \.offset) { _, check in
                        HStack(spacing: 6) {
                            StatusDot(tone: tone(check.result), label: check.result)
                            Text(verbatim: displaySafe(check.name)).font(.callout)
                            Spacer()
                            Text(verbatim: check.result).textStyle(.caption)
                        }
                    }
                }
                .padding(.top, 6)
            } label: {
                let s = detail.checkSummary
                Text(verbatim: "Checks — \(s.failing) failing, \(s.running) running, \(s.passing) passing").textStyle(.section)
            }
        }
    }

    private func tone(_ result: String) -> Tone {
        switch result {
        case "success", "neutral", "skipped": .good
        case "failure", "error", "timed_out", "cancelled", "action_required", "startup_failure": .bad
        default: .warning
        }
    }
}

private struct FilesCard: View {
    let detail: PRDetail
    @State private var expanded = false

    var body: some View {
        Card {
            DisclosureGroup(isExpanded: $expanded) {
                VStack(alignment: .leading, spacing: 3) {
                    ForEach(Array(detail.files.enumerated()), id: \.offset) { _, file in
                        HStack(spacing: 8) {
                            Text(verbatim: displaySafe(file.path)).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
                            Spacer()
                            Text(verbatim: "+\(file.additions)").font(.caption.monospacedDigit()).foregroundStyle(.green)
                            Text(verbatim: "−\(file.deletions)").font(.caption.monospacedDigit()).foregroundStyle(.red)
                        }
                    }
                    if detail.changedFiles > detail.files.count {
                        Text(verbatim: "and \(detail.changedFiles - detail.files.count) more on GitHub").textStyle(.caption)
                    }
                }
                .padding(.top, 6)
            } label: {
                Text(verbatim: "Files — \(detail.changedFiles)").textStyle(.section)
            }
        }
    }
}

/// Markdown from GitHub or a review, rendered the way Docs renders it: off the
/// main thread and inert (no images loaded, raw HTML shown as text, links
/// opened only after `MarkdownRenderer.linkURL` checks them). Too deeply
/// nested input is shown as its source.
struct MarkdownBlock: View {
    let source: String
    var base: URL?
    var minHeight: CGFloat = 60
    var maxHeight: CGFloat = 360

    @State private var text: NSAttributedString?

    var body: some View {
        Group {
            if let text {
                RichTextView(text: text)
                    .frame(height: height(text))
                    .clipShape(RoundedRectangle(cornerRadius: 6))
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: minHeight)
            }
        }
        .task(id: source) {
            let made = await MarkdownRenderer(baseURL: base).renderInBackground(source)
            guard !Task.isCancelled else { return }
            text = made?.text ?? NSAttributedString(string: displaySafeLines(source), attributes: [
                .font: NSFont.monospacedSystemFont(ofSize: NSFont.systemFontSize, weight: .regular),
                .foregroundColor: NSColor.labelColor,
            ])
        }
    }

    /// Tall enough for the text at a typical card width, within the bounds;
    /// longer text scrolls.
    private func height(_ text: NSAttributedString) -> CGFloat {
        let bounds = text.boundingRect(with: NSSize(width: 520, height: CGFloat.greatestFiniteMagnitude),
                                       options: [.usesLineFragmentOrigin, .usesFontLeading])
        return min(maxHeight, max(minHeight, ceil(bounds.height) + 28))
    }
}
