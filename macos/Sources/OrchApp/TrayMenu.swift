import OrchKit
import SwiftUI

struct TrayMenu: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let control: DaemonControl

    @State private var busy = false
    @State private var controlError: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            Divider()
            NeedsYouSection(monitor: prMonitor)
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
            actions
        }
        .padding(12)
        .frame(width: 320)
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: monitor.health.symbolName)
                .foregroundStyle(tint)
                .font(.title2)
            VStack(alignment: .leading, spacing: 2) {
                Text("Orchestrator").font(.headline)
                Text(monitor.health.summary)
                    .font(.caption)
                    .foregroundStyle(.secondary)
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

    private var tint: Color {
        switch monitor.health {
        case .healthy: .green
        case .degraded: .orange
        case .down: .red
        case .unknown: .secondary
        }
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

private struct SubsystemList: View {
    let subsystems: [Subsystem]

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            ForEach(subsystems) { unit in
                HStack(spacing: 6) {
                    Circle()
                        .fill(color(for: unit))
                        .frame(width: 7, height: 7)
                    Text(unit.name).font(.callout)
                    Spacer()
                    Text(unit.isFailed ? "failed" : unit.state)
                        .font(.caption)
                        .foregroundStyle(.secondary)
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
