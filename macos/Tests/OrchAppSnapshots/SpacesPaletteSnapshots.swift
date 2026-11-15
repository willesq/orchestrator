import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the ⌘K palette with its Spaces section and the companion column's
// pane strip with the Spaces badge (SP-055), in light and dark, from fixed
// sample data and without orchd (the companion's PRs and tasks show as
// unreachable):
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesPaletteSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesPaletteSnapshots {
    @Test func palette() async throws {
        let dir = try outputDir()
        let spaces = SpacesPalette.entries(spaces: [(id: "acme", name: "acme"), (id: "oss", name: "oss"),
                                                    (id: "personal", name: "personal")],
                                           spacesOff: false, shareSafe: nil)
        var commands = [
            PaletteCommand(id: "pane:dashboard", section: "Go to", title: "Dashboard", symbol: Pane.dashboard.symbol) {},
            PaletteCommand(id: "pane:spaces", section: "Go to", title: "Spaces", subtitle: "2 conflicts",
                           symbol: Pane.spaces.symbol, tone: .bad) {},
        ]
        commands += spaces.map { e in
            PaletteCommand(id: e.id, section: "Spaces", title: e.title, subtitle: e.subtitle, symbol: e.symbol) {}
        }
        for (name, appearance) in appearances {
            let size = NSSize(width: 600, height: 520)
            try await render(CommandPaletteView(commands: commands, dismiss: {}).padding(0), size: size, appearance: appearance,
                             to: dir.appendingPathComponent("palette-spaces-\(name).png"))
        }
    }

    @Test func companionStrip() async throws {
        let dir = try outputDir()
        _ = NSApplication.shared
        let daemon = UnreachableDaemon(reason: "sample: no orchd")
        let states: [(String, [Pane], SidebarStatus)] = [
            ("badge", Pane.allCases, .count(2, .bad, "2 conflicts")),
            ("spaces-off", Pane.allCases.filter { $0 != .spaces }, .none),
        ]
        for (state, panes, spacesStatus) in states {
            for (name, appearance) in appearances {
                let size = NSSize(width: 320, height: 640)
                let view = CompanionView(monitor: DaemonMonitor(api: daemon), prMonitor: PRMonitor(api: daemon),
                                         tasks: TasksModel(api: daemon), window: WindowControl(), panes: panes,
                                         status: { $0 == .spaces ? spacesStatus : .none }, open: { _ in },
                                         openPR: { _ in }, openTask: { _ in }, newTask: {})
                    .frame(width: size.width, height: size.height)
                    .background(Color(nsColor: .windowBackgroundColor))
                try await render(view, size: size, appearance: appearance,
                                 to: dir.appendingPathComponent("companion-\(state)-\(name).png"))
            }
        }
    }

    private let appearances: [(String, NSAppearance.Name)] = [("light", .aqua), ("dark", .darkAqua)]

    private func outputDir() throws -> URL {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        return dir
    }

    /// Draws `view` in an offscreen window and writes it as a PNG.
    private func render(_ view: some View, size: NSSize, appearance: NSAppearance.Name, to url: URL) async throws {
        let hosting = NSHostingView(rootView: view)
        hosting.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: appearance)
        window.contentView = hosting
        hosting.layoutSubtreeIfNeeded()
        try await Task.sleep(for: .milliseconds(500))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
