import OrchKit
import SwiftUI

enum Pane: String, CaseIterable, Identifiable {
    case dashboard, system, memory, docs, sources, tasks, github, claude, safetyNet

    var id: String { rawValue }

    var title: String {
        switch self {
        case .dashboard: "Dashboard"
        case .system: "System"
        case .memory: "Memory"
        case .docs: "Docs"
        case .sources: "Sources"
        case .tasks: "Tasks"
        case .github: "GitHub"
        case .claude: "Claude"
        case .safetyNet: "Safety Net"
        }
    }

    var symbol: String {
        switch self {
        case .dashboard: "gauge.with.dots.needle.33percent"
        case .system: "cpu"
        case .memory: "brain.head.profile"
        case .docs: "doc.text.magnifyingglass"
        case .sources: "tray.full"
        case .tasks: "checklist"
        case .github: "point.3.connected.trianglepath.dotted"
        case .claude: "slider.horizontal.3"
        case .safetyNet: "checkmark.shield"
        }
    }
}

struct MainWindow: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let restart: RestartModel
    let search: SearchModel
    let sources: SourcesModel
    let tasks: TasksModel
    let dispatch: DispatchModel
    let github: GitHubModel
    let prs: PRsModel
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel
    let claude: SettingsModel
    @Bindable var navigator: Navigator

    private var pane: Pane {
        get { navigator.pane }
        nonmutating set { navigator.pane = newValue }
    }

    var body: some View {
        NavigationSplitView {
            List(Pane.allCases, selection: Binding(get: { pane }, set: { pane = $0 ?? .dashboard })) { item in
                SidebarRow(pane: item, status: sidebarStatus(item), selected: item == pane).tag(item)
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190)
            .safeAreaInset(edge: .bottom) {
                AppearancePicker()
                    .pickerStyle(.menu)
                    .controlSize(.small)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
            }
        } detail: {
            switch pane {
            case .dashboard:
                DashboardView(monitor: monitor, prMonitor: prMonitor, restart: restart, docs: docs, safetyNet: safetyNet,
                              open: { pane = $0 })
            case .system:
                SystemView(model: system, logs: logs, restart: restart)
            case .memory:
                MemoryView(search: search, sources: sources)
            case .docs:
                DocsView(model: docs)
            case .sources:
                SourcesView(model: registeredSources)
            case .tasks:
                TasksView(model: tasks, dispatch: dispatch, navigator: navigator)
            case .github:
                GitHubView(model: github, prs: prs, navigator: navigator)
            case .claude:
                ClaudeView(model: claude)
            case .safetyNet:
                SafetyNetView(model: safetyNet, settings: claude)
            }
        }
        .frame(minWidth: 760, minHeight: 480)
        .background {
            // A hidden button, so ⌘K works from any pane without a menu bar.
            Button("Command palette") {
                PalettePanel.shared.toggle(over: NSApp.keyWindow, commands: paletteCommands)
            }
            .keyboardShortcut("k", modifiers: .command)
            .hidden()
        }
    }

    /// Everything the palette offers, most pressing first: what needs the
    /// user, then actions, panes, the rest of the PRs, and recent tasks.
    private var paletteCommands: [PaletteCommand] {
        var commands: [PaletteCommand] = []
        let needsYou = prMonitor.needsYou
        for pr in needsYou {
            commands.append(PaletteCommand(
                id: "needs:\(pr.url)", section: "Needs you", title: pr.title,
                subtitle: "\(pr.repo) · \(pr.attention?.label ?? "Needs you")",
                symbol: "exclamationmark.circle.fill", tone: .warning
            ) { navigator.showPR(pr.url, in: prs) })
        }
        commands.append(PaletteCommand(id: "action:dispatch", section: "Actions", title: "New task…",
                                       subtitle: "Dispatch work to a background agent", symbol: "paperplane") {
            navigator.dispatchRequested = true
            pane = .tasks
        })
        commands.append(PaletteCommand(id: "action:refresh", section: "Actions", title: "Refresh everything",
                                       subtitle: "Reload status, PRs, tasks, docs and settings", symbol: "arrow.clockwise") {
            refreshAll()
        })
        commands.append(PaletteCommand(id: "action:needs", section: "Actions", title: "Show PRs that need action",
                                       symbol: "line.3.horizontal.decrease.circle") {
            navigator.showNeedsAction(in: prs)
        })
        for choice in AppearanceChoice.allCases {
            commands.append(PaletteCommand(id: "appearance:\(choice.rawValue)", section: "Actions",
                                           title: "Appearance: \(choice.label)", symbol: "circle.lefthalf.filled") {
                UserDefaults.standard.set(choice.rawValue, forKey: AppearanceChoice.key)
                choice.apply()
            })
        }
        for item in Pane.allCases {
            var subtitle: String?
            var tone = Tone.neutral
            switch sidebarStatus(item) {
            case .none: break
            case .count(_, let t, let label), .dot(let t, let label):
                subtitle = label
                tone = t
            }
            commands.append(PaletteCommand(id: "pane:\(item.rawValue)", section: "Go to", title: item.title,
                                           subtitle: subtitle, symbol: item.symbol, tone: tone) { pane = item })
        }
        let waiting = Set(needsYou.map(\.url))
        for pr in prMonitor.list?.prs ?? [] where !waiting.contains(pr.url) {
            commands.append(PaletteCommand(id: "pr:\(pr.url)", section: "Pull requests", title: pr.title,
                                           subtitle: pr.repo + (pr.isDraft ? " · Draft" : ""),
                                           symbol: "arrow.triangle.pull") { navigator.showPR(pr.url, in: prs) })
        }
        for task in tasks.tasks.prefix(30) {
            let line = task.body.split(whereSeparator: \.isNewline).first.map(String.init) ?? task.id
            commands.append(PaletteCommand(id: "task:\(task.id)", section: "Tasks", title: line,
                                           subtitle: "\(task.status) · \(task.agent) · \(task.updatedAt)",
                                           symbol: "checklist") {
                tasks.selectedID = task.id
                pane = .tasks
            })
        }
        return commands
    }

    private func refreshAll() {
        Task {
            async let a: Void = monitor.refresh()
            async let b: Void = prMonitor.refresh()
            async let c: Void = github.refreshItems()
            async let d: Void = tasks.refresh()
            async let e: Void = docs.load()
            async let f: Void = safetyNet.load()
            async let g: Void = claude.load()
            _ = await (a, b, c, d, e, f, g)
        }
    }

    /// What a pane's sidebar row shows beside its name, from data the
    /// models already hold.
    private func sidebarStatus(_ pane: Pane) -> SidebarStatus {
        switch pane {
        case .docs:
            let failing = docs.catalog?.failingRepos.count ?? 0
            return failing > 0 ? .count(failing, .warning, "\(failing) repos failed to sync") : .none
        case .github:
            let n = prMonitor.needsYou.count
            return n > 0 ? .count(n, .accent, "\(n) need you") : .none
        case .safetyNet:
            guard let state = safetyNet.state else { return .none }
            let badge = state.badge
            return .dot(SafetyNetView.tone(badge.tone), badge.label)
        case .claude:
            let n = claude.changedOutside
            return n > 0 ? .dot(.warning, "\(n) edited outside orch") : .none
        default:
            return .none
        }
    }
}

/// A sidebar badge: a count, or a colored state dot.
enum SidebarStatus: Equatable {
    case none
    case count(Int, Tone, String)
    case dot(Tone, String)
}

private struct SidebarRow: View {
    let pane: Pane
    let status: SidebarStatus
    /// On the selection highlight a tinted badge would vanish, so it turns white.
    var selected = false

    var body: some View {
        HStack {
            Label(pane.title, systemImage: pane.symbol)
            Spacer(minLength: 4)
            switch status {
            case .none:
                EmptyView()
            case .count(let n, let tone, let label):
                Text(verbatim: n > 99 ? "99+" : "\(n)")
                    .font(.caption.weight(.semibold).monospacedDigit())
                    .padding(.horizontal, 6)
                    .padding(.vertical, 1)
                    .background(selected ? Color.white.opacity(0.25) : tone.color.opacity(0.22), in: Capsule())
                    .foregroundStyle(selected ? Color.white : tone.color)
                    .help(label)
                    .accessibilityLabel(label)
            case .dot(let tone, let label):
                // A symbol rather than a shape, so VoiceOver reads its label
                // with the row's name.
                Image(systemName: "circle.fill")
                    .font(.system(size: 8))
                    .foregroundStyle(tone.color)
                    .help(label)
                    .accessibilityLabel(label)
            }
        }
        .accessibilityElement(children: .combine)
    }
}
