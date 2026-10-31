import OrchKit
import SwiftUI

/// The Dashboard's record of what orch did on its own, newest first, grouped
/// by day. Each entry opens the task, PR, docs or source it records, so the
/// user can see and then steer what ran while they were away.
struct ActivityTimeline: View {
    let tasks: TasksModel
    let reviews: ReviewMonitor
    let docs: DocsModel
    let sources: RegisteredSourcesModel
    let follow: (ActivityTarget) -> Void

    @AppStorage("activityFilter") private var filterRaw = ""
    @State private var expanded = false

    /// How often the timeline reloads tasks and sources while it shows;
    /// reviews and docs keep themselves current app-wide.
    private static let reloadInterval: Duration = .seconds(15)
    private static let collapsedCount = 12

    private var filter: ActivityKind? { ActivityKind(rawValue: filterRaw) }

    private var allEvents: [ActivityEvent] {
        ActivityFeed.events(tasks: tasks.tasks, reviews: reviews.reviews ?? [], docs: docs.catalog,
                            sources: sources.sources)
    }

    var body: some View {
        let all = allEvents
        let shown = filter.map { kind in all.filter { $0.kind == kind } } ?? all
        let visible = expanded ? shown : Array(shown.prefix(Self.collapsedCount))
        Card("Activity") {
            if all.contains(where: { $0.outcome == .running }) { LiveBadge() }
        } content: {
            VStack(alignment: .leading, spacing: 10) {
                filterBar(all)
                if visible.isEmpty {
                    Text(filter == nil ? "Nothing yet. Tasks, review verdicts, docs syncs and source refreshes appear here as orch does them."
                                       : "No \(filter!.label.lowercased()) activity.")
                        .textStyle(.supporting)
                        .padding(.vertical, 8)
                } else {
                    // Relative times ("4m ago") age while the pane stays open.
                    TimelineView(.periodic(from: .now, by: 30)) { context in
                        rows(visible, now: context.date)
                    }
                }
                if shown.count > Self.collapsedCount {
                    Button(expanded ? "Show less" : "Show \(shown.count - Self.collapsedCount) more") {
                        withAnimation(.snappy) { expanded.toggle() }
                    }
                    .buttonStyle(.link)
                    .font(.callout)
                }
            }
        }
        .task {
            while !Task.isCancelled {
                async let a: Void = tasks.refresh()
                async let b: Void = sources.load()
                _ = await (a, b)
                try? await Task.sleep(for: Self.reloadInterval)
            }
        }
    }

    private func filterBar(_ all: [ActivityEvent]) -> some View {
        HStack(spacing: 6) {
            FilterPill(title: "All", count: all.count, selected: filter == nil) { filterRaw = "" }
            ForEach(ActivityKind.allCases) { kind in
                FilterPill(title: kind.label, count: all.filter { $0.kind == kind }.count, selected: filter == kind) {
                    filterRaw = kind.rawValue
                }
            }
        }
    }

    private func rows(_ events: [ActivityEvent], now: Date) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(events.enumerated()), id: \.element.id) { index, event in
                let day = ActivityFeed.day(event.date, now: now)
                if index == 0 || ActivityFeed.day(events[index - 1].date, now: now) != day {
                    Text(day.uppercased())
                        .font(.caption2.weight(.semibold))
                        .tracking(0.6)
                        .foregroundStyle(Color.tertiaryText)
                        .padding(.leading, 34)
                        .padding(.top, index == 0 ? 2 : 12)
                        .padding(.bottom, 4)
                        .accessibilityAddTraits(.isHeader)
                }
                ActivityRow(event: event, now: now, isFirst: index == 0, isLast: index == events.count - 1) {
                    follow(event.target)
                }
            }
        }
    }
}

private extension ActivityOutcome {
    var tone: Tone {
        switch self {
        case .done: .good
        case .failed: .bad
        case .running: .accent
        case .waiting: .neutral
        case .attention: .warning
        }
    }

    var label: String {
        switch self {
        case .done: "done"
        case .failed: "failed"
        case .running: "running"
        case .waiting: "waiting"
        case .attention: "needs a look"
        }
    }
}

/// One entry: a node on the rail, what happened, and when.
private struct ActivityRow: View {
    let event: ActivityEvent
    let now: Date
    let isFirst: Bool
    let isLast: Bool
    let open: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: open) {
            HStack(alignment: .top, spacing: 10) {
                node
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: event.title)
                        .font(.body)
                        .lineLimit(1)
                        .truncationMode(.tail)
                    Text(verbatim: event.detail)
                        .font(.caption)
                        .foregroundStyle(event.outcome == .failed ? Color.red : Color.secondaryText)
                        .lineLimit(1)
                }
                .padding(.vertical, 6)
                Spacer(minLength: 8)
                Text(relative)
                    .textStyle(.caption)
                    .monospacedDigit()
                    .padding(.top, 7)
                    .help(event.date.formatted(date: .abbreviated, time: .standard))
                Image(systemName: "chevron.right")
                    .font(.caption2.weight(.semibold))
                    .foregroundStyle(Color.tertiaryText)
                    .opacity(hovering ? 1 : 0)
                    .padding(.top, 9)
            }
            .padding(.trailing, 6)
            .contentShape(Rectangle())
            .background(hovering ? Color.primary.opacity(0.05) : .clear, in: RoundedRectangle(cornerRadius: 6, style: .continuous))
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(verbatim: "\(event.title), \(event.detail), \(relative)"))
        .accessibilityHint("Opens it")
    }

    /// The rail runs through every node; it stops at the first and last so
    /// the timeline reads as one bounded strand.
    private var node: some View {
        ZStack {
            VStack(spacing: 0) {
                Rectangle().fill(isFirst ? .clear : Color.surfaceStroke).frame(width: 2, height: 14)
                Spacer(minLength: 0)
                Rectangle().fill(isLast ? .clear : Color.surfaceStroke).frame(width: 2)
            }
            Image(systemName: event.symbol)
                .font(.system(size: 10, weight: .semibold))
                .foregroundStyle(.white)
                .frame(width: 22, height: 22)
                .background(event.outcome.tone.color, in: Circle())
                .overlay(Circle().strokeBorder(Color.raisedSurface, lineWidth: 2))
                .symbolEffect(.pulse, isActive: event.outcome == .running)
                .padding(.top, 5)
                .frame(maxHeight: .infinity, alignment: .top)
                .accessibilityLabel(event.outcome.label)
        }
        .frame(width: 24)
    }

    private var relative: String {
        let seconds = now.timeIntervalSince(event.date)
        if seconds < 45 { return "just now" }
        return event.date.formatted(.relative(presentation: .numeric, unitsStyle: .abbreviated))
    }
}

private struct FilterPill: View {
    let title: String
    let count: Int
    let selected: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 4) {
                Text(title)
                Text(verbatim: "\(count)")
                    .monospacedDigit()
                    .foregroundStyle(selected ? Color.white.opacity(0.8) : Color.tertiaryText)
            }
            .font(.caption.weight(.medium))
            .padding(.horizontal, 9)
            .padding(.vertical, 4)
            .foregroundStyle(selected ? Color.white : Color.primary)
            .background(selected ? Color.accentColor : Color.primary.opacity(0.06), in: Capsule())
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .disabled(count == 0 && !selected)
        .opacity(count == 0 && !selected ? 0.5 : 1)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}

/// Shown while anything on the timeline is still running.
private struct LiveBadge: View {
    var body: some View {
        HStack(spacing: 5) {
            Image(systemName: "circle.fill").font(.system(size: 7)).foregroundStyle(Color.accentColor)
                .symbolEffect(.pulse)
            Text("Live").font(.caption.weight(.semibold)).foregroundStyle(Color.accentColor)
        }
        .help("Something orch started is still running")
    }
}
