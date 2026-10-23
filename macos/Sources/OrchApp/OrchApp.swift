import OrchKit
import SwiftUI

@main
struct OrchApp: App {
    private let monitor: DaemonMonitor
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
    }

    var body: some Scene {
        MenuBarExtra {
            TrayMenu(monitor: monitor, control: control)
        } label: {
            Image(systemName: monitor.health.symbolName)
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
}
