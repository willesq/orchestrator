import OrchKit
import SwiftUI

/// The Spaces pane's Overview (SP-051): what needs the user, each space with
/// its bindings and counts, and the live sessions with their pins. Basic
/// hides detail, never something that needs the user. Screen-share-safe
/// mode shows one space and nothing of any other.
struct SpacesView: View {
    let model: SpacesOverviewModel
    let enforcement: EnforcementModel
    let navigator: Navigator
    /// Activity and Review (SP-054); without them the pane is the Overview.
    var activity: SpacesActivityModel?
    var review: SpacesReviewModel?
    @State private var page: SpacesPage = .overview
    /// "Relabel now" (SP-079); absent where none is given.
    @Environment(SpacesRelabelModel.self) private var relabel: SpacesRelabelModel?
    /// The advanced editors (SP-053); absent where none is given.
    @Environment(SpacesEditorModel.self) private var editor: SpacesEditorModel?
    /// The setup wizard while its sheet is open (SP-052).
    @State private var setup: SpacesSetupModel?
    /// What the wizard left to know after it closed, such as a saved draft.
    @State private var setupNote: String?

    var body: some View {
        VStack(spacing: 0) {
            // Screen-share-safe mode keeps to the Overview.
            let pages = model.shareSafe == nil && activity != nil && review != nil
            if pages {
                SpacesPagePicker(page: $page)
                Divider()
            }
            switch pages ? page : .overview {
            case .activity:
                if let activity { SpacesActivityView(model: activity) }
            case .review:
                if let review { SpacesReviewView(model: review, onApplied: { Task { await model.load() } }) }
            case .edit:
                if let editor {
                    SpacesEditorView(model: editor, enforcement: enforcement, navigator: navigator, openReview: { page = .review })
                }
            case .overview:
                SpacesHeader(model: model, runSetup: openSetup)
                Divider()
                if let note = setupNote {
                    Text(verbatim: note).textStyle(.caption).padding(.horizontal, 12).padding(.vertical, 6)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                content
            }
        }
        .navigationTitle("Spaces")
        .task { await model.load() }
        .sheet(isPresented: Binding(get: { setup != nil }, set: { if !$0 { setup = nil } })) {
            if let setup {
                SpacesSetupView(model: setup) { note in
                    self.setup = nil
                    setupNote = note
                    Task { await model.load() }
                }
            }
        }
    }

    /// Setup lists every space, so it never opens while sharing the screen.
    private func openSetup() {
        guard model.shareSafe == nil else { return }
        setupNote = nil
        setup = model.makeSetup()
    }

    @ViewBuilder private var content: some View {
        if let overview = model.overview {
            if !overview.configured && overview.shareSafe == nil {
                VStack(spacing: 0) {
                    EnforcementBanner(model: enforcement, show: { navigator.showSettings(.spaces) }).padding(12)
                    EmptyState(symbol: "square.3.layers.3d", title: "Spaces are not set up",
                               message: "With no spaces applied, orch keeps one shared memory, as before. Setup proposes spaces from your checkouts and GitHub orgs; nothing changes until you apply.",
                               actionTitle: "Set Up Spaces…", action: openSetup)
                }
            } else {
                AdaptiveSplit(primaryWidth: .init(min: 300, ideal: 460), secondaryWidth: .init(min: 300, ideal: 380),
                              stacking: .tabs(primary: "Spaces", secondary: "Sessions")) {
                    ScrollView {
                        VStack(alignment: .leading, spacing: Metrics.stackSpacing) {
                            // Settings stays out of reach while sharing: the
                            // validator answers for every space.
                            EnforcementBanner(model: enforcement,
                                              show: overview.shareSafe == nil ? { navigator.showSettings(.spaces) } : nil)
                            NeedsYouStrip(items: overview.needsYou, problems: overview.problems)
                            if let relabel, overview.shareSafe == nil { SpacesRelabelControl(model: relabel) }
                            SpaceCards(cards: overview.cards, shareSafe: overview.shareSafe)
                            if overview.shareSafe == nil {
                                Button("Where would this go?") { navigator.showSettings(.spaces) }
                                    .help("Check which space a folder's content goes to, and why")
                            }
                        }
                        .padding(Metrics.pagePadding)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    // Counts stale chunks when the pane shows and after each apply.
                    .task(id: overview.shareSafe == nil ? overview.appliedRevision : -1) {
                        if overview.shareSafe == nil { await relabel?.check() }
                    }
                } secondary: {
                    SessionList(sessions: overview.sessions, detail: model.detail, shareSafe: overview.shareSafe)
                }
            }
        } else if let error = model.lastError {
            EmptyState(symbol: "exclamationmark.triangle", title: "Spaces could not be read", message: error,
                       actionTitle: "Try Again", action: { Task { await model.load() } })
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

/// The policy's revision, Basic/Advanced, and screen-share-safe mode.
private struct SpacesHeader: View {
    let model: SpacesOverviewModel
    let runSetup: () -> Void

    var body: some View {
        // Wraps onto two lines at companion width rather than clipping.
        FlowLayout(spacing: 8, lineSpacing: 6) {
            Text(revision).textStyle(.supporting).lineLimit(1)
            Picker("Detail", selection: Binding(get: { model.detail }, set: { d in Task { await model.setDetail(d) } })) {
                ForEach(SpacesDetail.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
            .help("Basic hides detail; anything that needs you shows in both")
            if let safe = model.shareSafe {
                Chip(text: "Sharing only \(displaySafe(safe))", tone: .accent, symbol: "eye.slash",
                     help: "Screen-share-safe mode: every other space is hidden")
                Button("Show All Spaces") { Task { await model.showAll() } }
            } else {
                Menu("Screen-Share Safe…") {
                    ForEach(model.spaceChoices, id: \.id) { choice in
                        Button(choice.name) { Task { await model.shareOnly(choice.id) } }
                    }
                }
                .fixedSize()
                .disabled(model.spaceChoices.isEmpty)
                .help("Hide every space except one, and every other pane, while you share your screen")
                if model.overview?.configured == true {
                    Button("Run Setup…", action: runSetup)
                        .help("Scan again and propose spaces for new checkouts and orgs; nothing changes until you apply")
                }
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var revision: String {
        guard let o = model.overview, o.configured else { return "No policy applied" }
        var text = "Applied policy · rev \(o.appliedRevision)"
        if o.draftRevision > 0 { text += " · draft rev \(o.draftRevision)" }
        return text
    }
}

/// Conflicts, quarantine, drift, relabel progress and refusals.
private struct NeedsYouStrip: View {
    let items: [NeedsYouItem]
    let problems: [String]

    var body: some View {
        if !items.isEmpty || !problems.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(items) { item in
                    HStack(alignment: .top, spacing: 8) {
                        Image(systemName: symbol(item.kind)).foregroundStyle(tone(item.kind).color)
                            .accessibilityHidden(true)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(verbatim: item.title).font(.callout.weight(.semibold))
                            ForEach(item.details, id: \.self) { line in
                                // Folders are literals; drift is prose.
                                Text(verbatim: line).textStyle(item.kind == .drift ? .caption : .codeCaption)
                                    .lineLimit(2).truncationMode(.middle)
                            }
                        }
                    }
                    .accessibilityElement(children: .combine)
                }
                ForEach(problems, id: \.self) { problem in
                    Label { Text(verbatim: problem) } icon: { Image(systemName: "exclamationmark.triangle") }
                        .textStyle(.caption)
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.orange.opacity(0.10), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
        }
    }

    private func symbol(_ kind: NeedsYouItem.Kind) -> String {
        switch kind {
        case .conflict: "exclamationmark.octagon.fill"
        case .quarantine: "tray.full.fill"
        case .drift: "arrow.triangle.branch"
        case .relabel: "arrow.triangle.2.circlepath"
        case .refusals: "hand.raised.fill"
        }
    }

    private func tone(_ kind: NeedsYouItem.Kind) -> Tone {
        switch kind {
        case .conflict: .bad
        case .quarantine, .drift, .refusals: .warning
        case .relabel: .accent
        }
    }
}

/// One card per space, as many to a row as fit.
private struct SpaceCards: View {
    let cards: [SpaceCard]
    let shareSafe: String?

    var body: some View {
        if cards.isEmpty {
            Text(shareSafe == nil ? "The applied policy has no spaces." : "The space you chose is not in the applied policy.")
                .textStyle(.supporting)
        } else {
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 240), spacing: 12, alignment: .top)], alignment: .leading,
                      spacing: 12) {
                ForEach(cards) { SpaceCardView(card: $0) }
            }
        }
    }
}

private struct SpaceCardView: View {
    let card: SpaceCard

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline) {
                Text(verbatim: card.name).textStyle(.section).lineLimit(1)
                Spacer(minLength: 6)
                Chip(text: card.kindLabel, tone: tone)
            }
            ForEach(card.bindings) { b in
                VStack(alignment: .leading, spacing: 1) {
                    Text(b.label).textStyle(.caption)
                    Text(verbatim: b.values.joined(separator: ", ")).textStyle(.codeCaption).foregroundStyle(.primary)
                        .lineLimit(3).truncationMode(.middle)
                }
            }
            if !card.reads.isEmpty {
                Text(verbatim: "Also reads " + card.reads.joined(separator: ", ")).textStyle(.caption)
            }
            Divider()
            Text(verbatim: counts).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
        }
        .padding(Metrics.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .cardSurface()
        .accessibilityElement(children: .combine)
    }

    private var tone: Tone {
        switch card.kind {
        case "isolated": .warning
        case "private": .accent
        default: .good
        }
    }

    private var counts: String {
        var parts = [card.liveSessions == 1 ? "1 live session" : "\(card.liveSessions) live sessions",
                     "\(card.toolCalls) tool calls", "\(card.directoryMoves) directory moves"]
        if card.crossSpaceMoves > 0 { parts.append("\(card.crossSpaceMoves) into another space") }
        parts.append(card.refusals == 1 ? "1 refusal" : "\(card.refusals) refusals")
        return parts.joined(separator: " · ") + " (7 days)"
    }
}

/// The sessions with their pins, as rows that fit a narrow column.
private struct SessionList: View {
    let sessions: [SessionLine]
    let detail: SpacesDetail
    let shareSafe: String?

    var body: some View {
        if sessions.isEmpty {
            EmptyState(symbol: "terminal", title: "No sessions",
                       message: shareSafe == nil ? "Claude Code and Codex sessions show here once they start."
                           : "No session of this space is running.")
        } else {
            List(sessions) { s in
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 6) {
                        Text(verbatim: s.title).textStyle(.rowTitle).lineLimit(1)
                        Spacer(minLength: 4)
                        Chip(text: s.state, tone: stateTone(s.state))
                    }
                    HStack(spacing: 6) {
                        if !s.space.isEmpty { Chip(text: s.space, tone: s.space == "quarantine" ? .warning : .neutral) }
                        Text(verbatim: s.directory.isEmpty ? "not pinned" : s.directory).textStyle(.codeCaption)
                            .lineLimit(1).truncationMode(.middle)
                    }
                    Text(verbatim: line(s)).textStyle(.caption).lineLimit(1)
                }
                .padding(.vertical, 2)
                .accessibilityElement(children: .combine)
            }
        }
    }

    private func line(_ s: SessionLine) -> String {
        var parts = ["\(s.toolEvents) tool events", s.refusals == 1 ? "1 refusal" : "\(s.refusals) refusals"]
        if let at = s.lastActivity {
            parts.append("active " + at.formatted(.relative(presentation: .numeric, unitsStyle: .abbreviated)))
        }
        if detail == .advanced && s.pid > 0 { parts.append("PID \(s.pid)") }
        return parts.joined(separator: " · ")
    }

    private func stateTone(_ state: String) -> Tone {
        switch state {
        case "active": .good
        case "idle": .neutral
        case "unpinned": .warning
        default: .neutral
        }
    }
}

extension SpacesBadge {
    /// The badge as the sidebar row shows it.
    var sidebarStatus: SidebarStatus {
        switch self {
        case .none: .none
        case .notSetUp: .dot(.neutral, label)
        case .conflicts(let n): .count(n, .bad, label)
        case .quarantine(let n): .count(n, .warning, label)
        case .drift: .dot(.warning, label)
        case .relabel: .dot(.accent, label)
        }
    }
}
