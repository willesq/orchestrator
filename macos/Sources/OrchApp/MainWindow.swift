import OrchKit
import SwiftUI

enum Pane: String, CaseIterable, Identifiable {
    case dashboard, system, memory, spaces, docs, sources, tasks, github, settings

    var id: String { rawValue }

    var title: String {
        switch self {
        case .dashboard: "Dashboard"
        case .system: "System"
        case .memory: "Memory"
        case .spaces: "Spaces"
        case .docs: "Docs"
        case .sources: "Sources"
        case .tasks: "Tasks"
        case .github: "GitHub"
        case .settings: "Settings"
        }
    }

    var symbol: String {
        switch self {
        case .dashboard: "gauge.with.dots.needle.33percent"
        case .system: "cpu"
        case .memory: "brain.head.profile"
        case .spaces: "square.3.layers.3d"
        case .docs: "doc.text.magnifyingglass"
        case .sources: "tray.full"
        case .tasks: "checklist"
        case .github: "point.3.connected.trianglepath.dotted"
        case .settings: "gearshape"
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
    let handoff: PRHandoffModel
    let landings: PRLandingModel
    let system: SystemModel
    let logs: LogsModel
    let docs: DocsModel
    let registeredSources: RegisteredSourcesModel
    let safetyNet: SafetyNetModel
    let claude: SettingsModel
    let config: ConfigModel
    let docsOwners: DocsOwnersModel
    let enforcement: EnforcementModel
    let placement: PlacementValidator
    let spaces: SpacesOverviewModel
    let loginItem: LoginItem
    @Bindable var navigator: Navigator
    let window: WindowControl

    /// Whether the window is too narrow for the sidebar and a pane, so it
    /// shows the companion column instead.
    @State private var compact = false

    private var pane: Pane {
        get { navigator.pane }
        nonmutating set { navigator.pane = newValue }
    }

    /// The space screen-share-safe mode shows; while it is set the window
    /// shows the Spaces pane and nothing else, since every other pane mixes
    /// all spaces' PRs, tasks and memory.
    private var shareSafe: String? { spaces.shareSafe }

    var body: some View {
        Group {
            if shareSafe != nil && compact {
                SpacesView(model: spaces, enforcement: enforcement, navigator: navigator)
            } else if compact {
                CompanionView(monitor: monitor, prMonitor: prMonitor, tasks: tasks, window: window,
                              status: sidebarStatus, open: { show($0) },
                              openPR: { url in
                                  navigator.showPR(url, in: prs)
                                  window.expand()
                              },
                              openTask: { id in
                                  tasks.selectedID = id
                                  show(.tasks)
                              },
                              newTask: {
                                  navigator.dispatchRequested = true
                                  show(.tasks)
                              })
            } else {
                splitView
            }
        }
        // Anything a pane cannot fit is cut at the trailing edge, not split
        // around the centre, so the sidebar always stays in the window.
        .frame(minWidth: CompanionLayout.minimumSize.width, maxWidth: .infinity,
               minHeight: CompanionLayout.minimumSize.height, maxHeight: .infinity, alignment: .topLeading)
        .onGeometryChange(for: Bool.self) { CompanionLayout.isCompact(width: $0.size.width) } action: { compact = $0 }
        // Keeps the Spaces badge current while the pane is closed.
        .task { spaces.start() }
        .background {
            // Hidden buttons, so the shortcuts work from any pane without a menu bar.
            Button("Command palette") {
                // The palette lists every PR and task, whatever their space.
                guard shareSafe == nil else { return }
                PalettePanel.shared.toggle(over: NSApp.keyWindow, commands: paletteCommands)
            }
            .keyboardShortcut("k", modifiers: .command)
            .hidden()
            Button("Companion") { window.toggleDock() }
                .keyboardShortcut("d", modifiers: [.command, .shift])
                .hidden()
        }
    }

    /// Shows a pane, widening the window first when it is a companion column.
    private func show(_ target: Pane) {
        pane = target
        if compact { window.expand() }
    }

    private var splitView: some View {
        NavigationSplitView {
            List(shareSafe == nil ? Pane.allCases : [.spaces],
                 selection: Binding(get: { shareSafe == nil ? pane : .spaces }, set: { pane = $0 ?? .dashboard })) { item in
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
            switch shareSafe == nil ? pane : .spaces {
            case .dashboard:
                DashboardView(monitor: monitor, prMonitor: prMonitor, restart: restart, docs: docs, safetyNet: safetyNet,
                              tasks: tasks, reviews: prs.reviews, sources: registeredSources,
                              open: { pane = $0 }, openSafetyNet: { navigator.showSettings(.safetyNet) }, follow: follow)
            case .system:
                SystemView(model: system, logs: logs, restart: restart)
            case .memory:
                MemoryView(search: search, sources: sources)
            case .spaces:
                SpacesView(model: spaces, enforcement: enforcement, navigator: navigator)
            case .docs:
                DocsView(model: docs)
            case .sources:
                SourcesView(model: registeredSources)
            case .tasks:
                TasksView(model: tasks, dispatch: dispatch, navigator: navigator)
            case .github:
                GitHubView(model: github, prs: prs, navigator: navigator)
                    .environment(handoff)
                    .environment(landings)
                    .environment(\.openTask) { id in
                        tasks.selectedID = id
                        pane = .tasks
                    }
                    // Which PRs an agent is on shows in the list, board and detail.
                    .task { await handoff.follow() }
                    .task { await landings.follow() }
            case .settings:
                SettingsView(config: config, docsOwners: docsOwners, github: github, claude: claude, safetyNet: safetyNet,
                             loginItem: loginItem, enforcement: enforcement, placement: placement, navigator: navigator)
            }
        }
        .toolbar {
            // Trailing: a navigation item would take the sidebar's slot by
            // the traffic lights and pull the floating sidebar to the edge.
            ToolbarItem(placement: .primaryAction) {
                Button { window.dock() } label: {
                    Label("Companion", systemImage: "pip.enter")
                }
                .help("Dock as a companion column at the screen's edge, above other windows (⌘⇧D)")
            }
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
        commands.append(PaletteCommand(id: "action:companion", section: "Actions",
                                       title: window.isDocked ? "Expand the window" : "Dock as companion",
                                       subtitle: window.isDocked ? "Back to the full layout"
                                           : "A column at the screen's edge, above other windows",
                                       symbol: window.isDocked ? "pip.exit" : "pip.enter") {
            window.toggleDock()
        })
        commands.append(PaletteCommand(id: "action:pin", section: "Actions",
                                       title: window.isPinned ? "Stop keeping Orch on top" : "Keep Orch on top",
                                       symbol: window.isPinned ? "pin.slash" : "pin") {
            window.togglePinned()
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
        for section in SettingsSection.allCases {
            commands.append(PaletteCommand(id: "settings:\(section.rawValue)", section: "Go to", title: "Settings: \(section.title)",
                                           symbol: section.symbol) { navigator.showSettings(section) })
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

    /// Opens what a Dashboard timeline entry records.
    private func follow(_ target: ActivityTarget) {
        switch target {
        case .task(let id):
            tasks.selectedID = id
            pane = .tasks
        case .pr(let url):
            navigator.showPR(url, in: prs)
        case .docs:
            pane = .docs
        case .source(let id):
            registeredSources.select(id)
            pane = .sources
        }
    }

    private func refreshAll() {
        Task {
            async let a: Void = monitor.refresh()
            async let b: Void = prMonitor.refresh()
            async let c: Void = github.checkGitHub()
            async let d: Void = tasks.refresh()
            async let e: Void = docs.load()
            async let f: Void = safetyNet.load()
            async let g: Void = claude.load()
            async let h: Void = config.load()
            async let i: Void = spaces.load()
            _ = await (a, b, c, d, e, f, g, h, i)
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
        case .spaces:
            return spaces.badge.sidebarStatus
        case .settings:
            return SettingsView.status(config: config, safetyNet: safetyNet, claude: claude)
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
