import OrchKit
import SwiftUI

struct DashboardView: View {
    let monitor: DaemonMonitor
    let prMonitor: PRMonitor
    let restart: RestartModel

    @State private var confirmRestartAll = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                header
                subsystems
                GroupBox("Needs you") {
                    NeedsYouSection(monitor: prMonitor)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(4)
                }
            }
            .padding(20)
        }
        .navigationTitle("Dashboard")
        .toolbar {
            Button {
                Task {
                    await monitor.refresh()
                    await prMonitor.refresh()
                }
            } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            Image(systemName: monitor.health.symbolName).font(.largeTitle)
            VStack(alignment: .leading) {
                Text("orchd").font(.title2.bold())
                Text(monitor.health.summary).foregroundStyle(.secondary)
            }
        }
    }

    @ViewBuilder private var subsystems: some View {
        if let status = monitor.health.status {
            GroupBox {
                VStack(alignment: .leading, spacing: 8) {
                    HStack {
                        Text("Subsystems").font(.headline)
                        Spacer()
                        Button("Restart all…") { confirmRestartAll = true }
                            .disabled(restart.restarting != nil)
                            .confirmationDialog(
                                "Restart every subsystem?", isPresented: $confirmRestartAll
                            ) {
                                Button("Restart all", role: .destructive) { Task { await restart.restart(nil) } }
                            } message: {
                                Text("In-flight indexing and queued work pause while each unit restarts.")
                            }
                    }
                    Divider()
                    ForEach(status.subsystems) { unit in
                        SubsystemRow(unit: unit, restart: restart)
                    }
                    if let message = restart.lastMessage {
                        Text(message)
                            .font(.caption)
                            .foregroundStyle(restart.lastFailed ? .red : .secondary)
                    }
                }
                .padding(4)
            }
        }
    }
}

private struct SubsystemRow: View {
    let unit: Subsystem
    let restart: RestartModel

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Circle()
                .fill(unit.isFailed ? Color.red : unit.state == "running" ? .green : .secondary)
                .frame(width: 8, height: 8)
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: unit.name).font(.body.monospaced())
                Text(verbatim: unit.error.isEmpty ? unit.detail : unit.error)
                    .font(.caption)
                    .foregroundStyle(unit.error.isEmpty ? Color.secondary : Color.red)
                    .lineLimit(2)
            }
            Spacer()
            if unit.restarts > 0 {
                Text("\(unit.restarts) restart\(unit.restarts == 1 ? "" : "s")")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Text(verbatim: unit.isFailed ? "failed" : unit.state)
                .font(.caption)
                .foregroundStyle(.secondary)
                .frame(width: 56, alignment: .trailing)
            if restart.restarting == unit.name {
                ProgressView().controlSize(.small).frame(width: 64)
            } else {
                Button("Restart") { Task { await restart.restart(unit.name) } }
                    .controlSize(.small)
                    .disabled(restart.restarting != nil)
                    .frame(width: 64)
            }
        }
    }
}
