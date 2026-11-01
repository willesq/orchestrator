import AppKit
import OrchKit
import SwiftUI

/// The PRs pane as a board: every PR the filters keep, in a lane for where it
/// stands on its way to merge, so what blocks each one shows without opening
/// it. The selected PR's detail and review open beside the board.
struct PRBoardView: View {
    let github: GitHubModel
    @Bindable var prs: PRsModel

    /// Cold PRs are folded away until asked for: they rarely need anything.
    @State private var showCold = false

    var body: some View {
        let all = github.items?.items ?? []
        AdaptiveSplit(primaryWidth: .init(min: 380), secondaryWidth: .init(min: 340, ideal: 440, max: 620),
                      stacking: .detail(shown: prs.selection != nil, backTitle: "Board", back: { prs.select(nil) }),
                      secondaryPresent: prs.selection != nil) {
            VStack(spacing: 0) {
                PRFilterBar(github: github, prs: prs, repos: prRepos(all))
                Divider()
                Group {
                    if github.items == nil, let error = github.lastError {
                        EmptyState(symbol: "exclamationmark.triangle", title: "PRs unavailable", message: error,
                                   actionTitle: "Try again", action: { Task { await github.refreshItems() } })
                    } else if github.items == nil {
                        ProgressView()
                    } else {
                        let visible = prs.visible(all)
                        if visible.isEmpty {
                            PRsEmptyState(prs: prs)
                        } else {
                            board(visible)
                        }
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        } secondary: {
            PRDetailView(prs: prs, login: github.identity?.login)
                .overlay(alignment: .topTrailing) {
                    Button { prs.select(nil) } label: {
                        Image(systemName: "xmark.circle.fill").imageScale(.large)
                    }
                    .buttonStyle(.plain)
                    .foregroundStyle(Color.secondaryText)
                    .padding(10)
                    .help("Close the PR (Esc)")
                    .accessibilityLabel("Close PR details")
                }
        }
        .onExitCommand { prs.select(nil) }
    }

    private func board(_ visible: [GitHubItem]) -> some View {
        let reviews = prs.reviews.byURL
        let lanes = prBoard(visible, reviews: reviews)
        return GeometryReader { geometry in
            let width = laneWidth(available: geometry.size.width)
            ScrollView(.horizontal) {
                HStack(alignment: .top, spacing: 12) {
                    ForEach(PRLane.allCases) { lane in
                        let items = lanes[lane] ?? []
                        if lane == .cold && !showCold {
                            FoldedLane(lane: lane, count: items.count) { withAnimation(.snappy) { showCold = true } }
                        } else {
                            LaneColumn(lane: lane, width: width, items: items, reviews: reviews, selection: prs.selection,
                                       select: { prs.select($0) },
                                       fold: lane == .cold ? { withAnimation(.snappy) { showCold = false } } : nil)
                        }
                    }
                }
                .padding(12)
                .frame(minHeight: geometry.size.height, alignment: .top)
            }
        }
    }

    /// Lanes share the width so every lane shows at once, scrolling sideways
    /// only when that would make cards too narrow to read.
    private func laneWidth(available: CGFloat) -> CGFloat {
        let open = CGFloat(showCold ? PRLane.allCases.count : PRLane.allCases.count - 1)
        let fixed = 24 + 12 * CGFloat(PRLane.allCases.count - 1) + (showCold ? 0 : FoldedLane.width)
        return min(max((available - fixed) / open, 172), 340)
    }
}

extension PRLane {
    var tone: Tone {
        switch self {
        case .fix: .bad
        case .flight: .accent
        case .review: .warning
        case .ship: .good
        case .cold: .neutral
        }
    }
}

/// One lane: its name and count, and its PRs as cards.
private struct LaneColumn: View {
    let lane: PRLane
    let width: CGFloat
    let items: [GitHubItem]
    let reviews: [String: PRReview]
    let selection: String?
    let select: (String) -> Void
    var fold: (() -> Void)?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            if items.isEmpty {
                Text("Nothing here")
                    .textStyle(.caption)
                    .frame(maxWidth: .infinity, minHeight: 64)
                    .overlay(
                        RoundedRectangle(cornerRadius: 8)
                            .strokeBorder(Color.surfaceStroke, style: StrokeStyle(lineWidth: 1, dash: [4, 3]))
                    )
            } else {
                ScrollView {
                    LazyVStack(spacing: 8) {
                        ForEach(items) { item in
                            PRCard(item: item, review: reviews[item.url], lane: lane,
                                   selected: selection == item.url) { select(item.url) }
                        }
                    }
                    .padding(1)
                }
                .scrollIndicators(.automatic)
            }
        }
        .padding(10)
        .frame(width: width, alignment: .top)
        .frame(maxHeight: .infinity, alignment: .top)
        .background(lane.tone.color.opacity(0.05), in: RoundedRectangle(cornerRadius: Metrics.cardRadius))
        .overlay(RoundedRectangle(cornerRadius: Metrics.cardRadius).strokeBorder(Color.surfaceStroke))
    }

    private var header: some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Image(systemName: lane.symbolName).foregroundStyle(lane.tone.color)
                    Text(lane.title).font(.headline)
                    Text("\(items.count)")
                        .font(.caption.weight(.semibold)).monospacedDigit()
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(lane.tone.color.opacity(0.18), in: Capsule())
                        .foregroundStyle(lane.tone.color)
                }
                Text(lane.blurb).textStyle(.caption).lineLimit(1)
            }
            .help(lane.blurb)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("\(lane.title), \(items.count) PR\(items.count == 1 ? "" : "s"). \(lane.blurb)")
            .accessibilityAddTraits(.isHeader)
            Spacer(minLength: 0)
            if let fold {
                Button(action: fold) { Image(systemName: "chevron.left.2") }
                    .buttonStyle(.plain)
                    .foregroundStyle(Color.secondaryText)
                    .help("Fold the \(lane.title) lane")
                    .accessibilityLabel("Fold the \(lane.title) lane")
            }
        }
    }
}

/// A lane folded to a narrow strip that shows only how many PRs it holds.
private struct FoldedLane: View {
    static let width: CGFloat = 44

    let lane: PRLane
    let count: Int
    let unfold: () -> Void

    var body: some View {
        Button(action: unfold) {
            VStack(spacing: 8) {
                Image(systemName: lane.symbolName)
                Text("\(count)").font(.headline).monospacedDigit()
                Text(lane.title)
                    .font(.caption.weight(.medium))
                    .fixedSize()
                    .rotationEffect(.degrees(90))
                    .frame(width: 20, height: 60)
                Spacer()
            }
            .padding(.vertical, 12)
            .frame(width: Self.width)
            .frame(maxHeight: .infinity)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .foregroundStyle(Color.secondaryText)
        .background(Color.raisedSurface, in: RoundedRectangle(cornerRadius: Metrics.cardRadius))
        .overlay(RoundedRectangle(cornerRadius: Metrics.cardRadius).strokeBorder(Color.surfaceStroke))
        .help("\(lane.blurb). Show the \(count) PR\(count == 1 ? "" : "s") here.")
        .accessibilityLabel("\(lane.title) lane, \(count) PRs, folded")
        .accessibilityHint("Shows the lane")
    }
}

/// One PR on the board: where it is, its title, and what it is waiting on.
private struct PRCard: View {
    let item: GitHubItem
    let review: PRReview?
    let lane: PRLane
    let selected: Bool
    let select: () -> Void

    @Environment(PRHandoffModel.self) private var handoff
    @Environment(PRLandingModel.self) private var landings

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                // The owner is in the hover link; narrow lanes need the room for the repo.
                Text(verbatim: displaySafe("\(item.repo.split(separator: "/").last ?? "")#\(item.number)"))
                    .lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 4)
                if let when = item.updatedAt {
                    Text(when, format: .relative(presentation: .numeric, unitsStyle: .abbreviated))
                        .foregroundStyle(Color.tertiaryText)
                }
            }
            .font(.caption)
            .foregroundStyle(Color.secondaryText)
            Text(verbatim: displaySafe(item.title))
                .font(.callout.weight(.medium))
                .lineLimit(3)
                .foregroundStyle(lane == .cold ? Color.secondaryText : Color.primary)
                .frame(maxWidth: .infinity, alignment: .leading)
            chips
        }
        .padding(10)
        .padding(.leading, 3)
        .background(selected ? Color.accentColor.opacity(0.12) : Color(nsColor: .controlBackgroundColor),
                    in: RoundedRectangle(cornerRadius: 8))
        .overlay(alignment: .leading) {
            UnevenRoundedRectangle(topLeadingRadius: 8, bottomLeadingRadius: 8)
                .fill(lane.tone.color)
                .frame(width: 3)
        }
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(selected ? Color.accentColor : Color.surfaceStroke, lineWidth: selected ? 2 : 1)
        )
        .shadow(color: .black.opacity(selected ? 0.12 : 0.05), radius: selected ? 4 : 1.5, y: 1)
        .contentShape(RoundedRectangle(cornerRadius: 8))
        .onTapGesture(perform: select)
        .contextMenu {
            Button("Show details", action: select)
            PRLinkMenu(item: item)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(verbatim: displaySafe("\(item.title), \(item.repo)#\(item.number)")))
        .accessibilityValue(accessibilityStatus)
        .accessibilityAddTraits(selected ? [.isButton, .isSelected] : .isButton)
        .accessibilityAction(.default, select)
        .help(item.webURL?.absoluteString ?? displaySafe(item.url))
    }

    /// What blocks or carries the PR; the attention chip already says when CI fails.
    @ViewBuilder private var chips: some View {
        let ci = ciChip
        let agent = handoff.working(on: item.url)
        if item.attention != nil || item.isDraft || ci != nil || review != nil || agent != nil {
            HStack(spacing: 4) {
                if let attention = item.attention {
                    Chip(text: attention.label, tone: attention == .readyToMerge ? .good : .bad,
                         symbol: attention.symbolName)
                }
                if item.isDraft { Chip(text: "Draft") }
                if let ci { ci }
                if let review { ReviewBadge(review: review) }
                if let agent {
                    AgentChip(task: agent)
                } else if landings.awaiting(in: handoff.tasks(for: item.url)) != nil {
                    ReadyToLandChip()
                }
            }
            .lineLimit(1)
        }
    }

    private var accessibilityStatus: String {
        var parts = [lane.title]
        if let attention = item.attention { parts.append(attention.label) }
        if item.isDraft { parts.append("Draft") }
        switch item.ciState {
        case "success": parts.append("CI passing")
        case "pending": parts.append("CI running")
        case "failure" where item.attention != .ciFailed: parts.append("CI failing")
        default: break
        }
        return parts.joined(separator: ", ")
    }

    private var ciChip: Chip? {
        switch item.ciState {
        case "success": Chip(text: "CI", tone: .good, symbol: "checkmark.circle", help: "CI passing")
        case "pending": Chip(text: "CI", tone: .accent, symbol: "clock", help: "CI running")
        case "failure" where item.attention != .ciFailed:
            Chip(text: "CI", tone: .bad, symbol: "xmark.octagon", help: "CI failing")
        default: nil
        }
    }
}
