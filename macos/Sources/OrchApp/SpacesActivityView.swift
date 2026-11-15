import OrchKit
import SwiftUI

/// The Spaces pane's pages. Screen-share-safe mode shows only the Overview:
/// the other two name every space's sessions and bindings.
enum SpacesPage: String, CaseIterable, Identifiable {
    case overview, activity, review, edit

    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: "Overview"
        case .activity: "Activity"
        case .review: "Review"
        case .edit: "Edit"
        }
    }
}

/// The page switcher above the Spaces pane.
struct SpacesPagePicker: View {
    @Binding var page: SpacesPage

    var body: some View {
        Picker("Page", selection: $page) {
            ForEach(SpacesPage.allCases) { Text($0.title).tag($0) }
        }
        .pickerStyle(.segmented)
        .labelsHidden()
        .fixedSize()
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// Activity (SP-054): each session's command log, with its directory moves
/// and refusals.
struct SpacesActivityView: View {
    let model: SpacesActivityModel

    var body: some View {
        Group {
            if model.sessions.isEmpty {
                if let error = model.lastError {
                    EmptyState(symbol: "exclamationmark.triangle", title: "Activity could not be read", message: error,
                               actionTitle: "Try Again", action: { Task { await model.load() } })
                } else if model.isLoading {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    EmptyState(symbol: "terminal", title: "No sessions",
                               message: "Claude Code and Codex sessions show here once they start, with every tool call they make.")
                }
            } else {
                AdaptiveSplit(primaryWidth: .init(min: 240, ideal: 280, max: 360), secondaryWidth: .init(min: 360, ideal: 620),
                              stacking: .tabs(primary: "Sessions", secondary: "Log")) {
                    ActivitySessionList(model: model)
                } secondary: {
                    ActivityLog(model: model)
                }
            }
        }
        .task { await model.load() }
    }
}

private struct ActivitySessionList: View {
    let model: SpacesActivityModel

    var body: some View {
        List(selection: Binding(get: { model.selected }, set: { id in
            if let id { Task { await model.select(id) } }
        })) {
            ForEach(model.sessions) { s in
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 6) {
                        Text(verbatim: s.title).textStyle(.rowTitle).lineLimit(1)
                        Spacer(minLength: 4)
                        Chip(text: s.state, tone: s.state == "active" ? .good : .neutral)
                    }
                    HStack(spacing: 6) {
                        if !s.space.isEmpty { Chip(text: s.space, tone: s.space == "quarantine" ? .warning : .neutral) }
                        Text(verbatim: s.directory).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
                    }
                    Text(verbatim: "\(s.toolEvents) tool events · " + (s.refusals == 1 ? "1 refusal" : "\(s.refusals) refusals"))
                        .textStyle(.caption)
                }
                .padding(.vertical, 2)
                .tag(s.id)
                .accessibilityElement(children: .combine)
            }
        }
    }
}

private struct ActivityLog: View {
    let model: SpacesActivityModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider()
            if let error = model.lastError {
                Label { Text(verbatim: error) } icon: { Image(systemName: "exclamationmark.triangle") }
                    .textStyle(.caption).padding(12)
            }
            if model.visibleRows.isEmpty && !model.isLoading {
                EmptyState(symbol: "list.bullet.rectangle", title: emptyTitle)
            } else {
                List {
                    ForEach(model.visibleRows) { ActivityRowView(row: $0) }
                    if model.more {
                        Button("Load More") { Task { await model.loadMore() } }
                            .help("Read the next records of this session's log")
                    }
                }
            }
            Text("Command text is credential-redacted and capped before orchd stores it. The log is hash-chained per session and never indexed into memory.")
                .textStyle(.caption).padding(.horizontal, 12).padding(.vertical, 8)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private var header: some View {
        FlowLayout(spacing: 8, lineSpacing: 6) {
            if let s = model.selectedSession {
                Text(verbatim: s.title).textStyle(.section).lineLimit(1)
                Text(verbatim: "pinned " + s.directory).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
            }
            Picker("Show", selection: Binding(get: { model.filter }, set: { model.filter = $0 })) {
                ForEach(ActivityFilter.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
            Button { Task { await model.load() } } label: { Image(systemName: "arrow.clockwise") }
                .help("Read the sessions and this log again")
                .accessibilityLabel("Refresh")
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private var emptyTitle: String {
        switch model.filter {
        case .all: "No tool calls logged"
        case .moves: "No directory moves"
        case .refusals: "No refusals"
        }
    }
}

private struct ActivityRowView: View {
    let row: ActivityRow

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                Text(row.at, format: .dateTime.hour().minute().second()).textStyle(.caption).monospacedDigit()
                Text(verbatim: row.title).textStyle(.rowTitle).lineLimit(1)
                if !row.agent.isEmpty { Chip(text: "subagent " + row.agent) }
                Spacer(minLength: 4)
                if !row.decision.isEmpty { Chip(text: row.decision, tone: tone, symbol: symbol) }
            }
            if !row.directory.isEmpty && row.kind != .move {
                Text(verbatim: row.directory).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
            }
            if !row.detail.isEmpty {
                // Commands, targets and folders are literals; a lost-events
                // note is prose.
                Text(verbatim: row.detail).font(row.kind == .gap ? .caption : .caption.monospaced())
                    .lineLimit(3).truncationMode(.tail)
                    .textSelection(.enabled)
            }
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
    }

    private var tone: Tone {
        switch row.verdict {
        case .refused: .bad
        case .wouldRefuse, .notEnforced: .warning
        case .logged, .none: .neutral
        }
    }

    private var symbol: String? {
        switch row.verdict {
        case .refused: "hand.raised.fill"
        case .wouldRefuse: "exclamationmark.triangle"
        default: nil
        }
    }
}
