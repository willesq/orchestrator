import OrchKit
import ServiceManagement
import SwiftUI

struct TrayMenu: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let loginItem: LoginItem
    let control: DaemonControl
    let enforcement: EnforcementModel
    /// Operator unlocks to confirm, deny or lock (SP-077).
    var unlocks: UnlocksModel?
    let openMainWindow: () -> Void
    /// Opens the main window on Settings → Spaces.
    var openSpaces: () -> Void = {}

    @State private var busy = false

    /// Whether memory spaces are switched off: no enforcement banner or
    /// switches then, as before spaces.
    private var spacesOff: Bool { monitor.health.status?.spacesOff ?? false }
    @State private var controlError: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            Divider()
            if !spacesOff {
                EnforcementBanner(model: enforcement, show: nil)
            }
            if let unlocks { UnlocksSection(model: unlocks) }
            NeedsYouSection(monitor: prMonitor)
            if !spacesOff, let switches = enforcement.state?.switches, !switches.isEmpty {
                Divider()
                TraySpaces(switches: switches, model: enforcement, openSpaces: openSpaces)
            }
            if let status = monitor.health.status {
                Divider()
                SubsystemList(subsystems: status.subsystems)
            }
            if let controlError {
                Text(controlError)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Divider()
            HStack {
                Button("Open Orchestrator…", action: openMainWindow)
                Spacer()
                Button("About Orch") { AboutPanel.show() }
                    .buttonStyle(.link)
            }
            LoginItemRow(item: loginItem)
            AppearancePicker()
                .pickerStyle(.segmented)
                .controlSize(.small)
            actions
        }
        .padding(12)
        .frame(width: 320)
        // macOS can change login items from System Settings while we run.
        .onAppear {
            loginItem.refresh()
            Task { await enforcement.load() }
        }
        .enforcementConfirmation(enforcement)
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 8) {
            HealthMark(health: monitor.health, size: 30)
            VStack(alignment: .leading, spacing: 2) {
                Text("Orchestrator").font(.headline)
                Text(monitor.health.summary)
                    .font(.caption)
                    .foregroundStyle(Color.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private var actions: some View {
        HStack {
            if case .down = monitor.health {
                Button("Start orchd") { run(.start) }
            } else {
                Button("Restart orchd") { run(.restart) }
            }
            Button("Refresh") {
                Task {
                    await monitor.refresh()
                    await prMonitor.refresh()
                }
            }
            Spacer()
            if busy { ProgressView().controlSize(.small) }
            Button("Quit") { NSApplication.shared.terminate(nil) }
        }
        .disabled(busy)
    }

    private func run(_ action: DaemonControl.Action) {
        busy = true
        controlError = nil
        Task {
            do {
                try await control.perform(action)
            } catch {
                controlError = error.localizedDescription
            }
            await monitor.refresh()
            busy = false
        }
    }
}

/// The enforcement switches, compact, with a way to the full section.
struct TraySpaces: View {
    let switches: [EnforcementSwitch]
    let model: EnforcementModel
    let openSpaces: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("Enforcement").font(.caption.weight(.semibold)).foregroundStyle(Color.secondaryText)
                Spacer()
                Button("Spaces Settings…", action: openSpaces).buttonStyle(.link).font(.caption)
            }
            ForEach(switches) { EnforcementRow(layer: $0, model: model, compact: true) }
        }
    }
}

private struct SubsystemList: View {
    let subsystems: [Subsystem]

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            ForEach(subsystems) { unit in
                HStack(spacing: 6) {
                    Circle()
                        .fill(color(for: unit))
                        .frame(width: 7, height: 7)
                    Text(verbatim: unit.name).font(.callout)
                    Spacer()
                    Text(verbatim: unit.isFailed ? "failed" : unit.state)
                        .font(.caption)
                        .foregroundStyle(Color.secondaryText)
                }
                .help(unit.error.isEmpty ? unit.detail : unit.error)
            }
        }
    }

    private func color(for unit: Subsystem) -> Color {
        if unit.isFailed { return .red }
        return unit.state == "running" ? .green : .secondary
    }
}

struct LoginItemRow: View {
    let item: LoginItem

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle("Open at login", isOn: Binding(get: { item.isEnabled }, set: { item.setEnabled($0) }))
                .toggleStyle(.checkbox)
                .disabled(item.state == .unavailable)
            switch item.state {
            case .needsApproval:
                Button("Allow in System Settings → Login Items") {
                    SMAppService.openSystemSettingsLoginItems()
                }
                .buttonStyle(.link)
                .font(.caption)
            case .unavailable:
                Text("Only available when running Orch.app.")
                    .font(.caption)
                    .foregroundStyle(Color.secondaryText)
            case .enabled, .disabled:
                EmptyView()
            }
            if let error = item.lastError {
                Text(error).font(.caption).foregroundStyle(.red)
            }
        }
    }
}
