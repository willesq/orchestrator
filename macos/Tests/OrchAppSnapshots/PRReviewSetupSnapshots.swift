import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the PR review card's setup section, as it starts and set for a
// stacked PR, in light and dark, to the folder ORCH_SNAPSHOT_DIR names:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter PRReviewSetupSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR.

private let reviewSetupSnapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

@MainActor
@Suite(.enabled(if: reviewSetupSnapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct PRReviewSetupSnapshots {
    @Test func reviewSetup() throws {
        let dir = URL(fileURLWithPath: try #require(reviewSetupSnapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let stacked = PRReviewSetup(instructionsFrom: .base, compareAgainst: .defaultBranch, projectHooks: .off)
        let view = VStack(alignment: .leading, spacing: 16) {
            ReviewSetupSection(setup: .standard) { _ in }
            Divider()
            ReviewSetupSection(setup: stacked) { _ in }
        }
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            try render(
                view.padding(16).frame(width: 520, height: 300, alignment: .top)
                    .background(Color(nsColor: .windowBackgroundColor)),
                size: NSSize(width: 520, height: 300), appearance: appearance,
                to: dir.appendingPathComponent("pr-review-setup-\(name).png")
            )
        }
    }

    /// Draws `view` in an offscreen window and writes it as a PNG.
    private func render(_ view: some View, size: NSSize, appearance: NSAppearance.Name, to url: URL) throws {
        let hosting = NSHostingView(rootView: view)
        hosting.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: appearance)
        window.contentView = hosting
        hosting.layoutSubtreeIfNeeded()
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        try #require(rep.representation(using: .png, properties: [:])).write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
