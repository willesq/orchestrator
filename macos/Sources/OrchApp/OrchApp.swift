import OrchKit
import SwiftUI

@main
struct OrchApp: App {
    private let monitor: DaemonMonitor
    private let prMonitor: PRMonitor
    private let notifier = Notifier()
    private let control = DaemonControl()

    init() {
        let api: any DaemonAPI
        do {
            let client = try DaemonClient()
            Task.detached { try? await client.run() }
            api = client
        } catch {
            api = UnreachableDaemon(reason: error.localizedDescription)
        }
        monitor = DaemonMonitor(api: api)
        monitor.start()
        prMonitor = PRMonitor(api: api)
        prMonitor.onNewlyNeedingYou = { [notifier] prs in notifier.post(prs) }
        prMonitor.start()
    }

    var body: some Scene {
        MenuBarExtra {
            TrayMenu(monitor: monitor, prMonitor: prMonitor, control: control)
        } label: {
            let icon = TrayIcon(health: monitor.health, needsYou: prMonitor.needsYou.count)
            HStack(spacing: 2) {
                Image(systemName: icon.symbolName)
                if let count = icon.count { Text("\(count)") }
            }
            .accessibilityLabel(monitor.health.summary)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Stands in when the gRPC transport cannot even be built, so the tray still
/// shows the daemon as down instead of the app failing to launch.
private struct UnreachableDaemon: DaemonAPI {
    let reason: String
    func status() async throws -> DaemonStatus {
        throw DaemonControl.ControlError.failed(status: -1, output: reason)
    }

    func listPRs() async throws -> PRList {
        throw DaemonControl.ControlError.failed(status: -1, output: reason)
    }
}
