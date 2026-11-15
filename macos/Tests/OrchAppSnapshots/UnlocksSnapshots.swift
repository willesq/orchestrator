import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the menu bar panel's operator unlocks (SP-077), a pending request
// and an active unlock, in light and dark, from sample data and without
// orchd, to the folder ORCH_SNAPSHOT_DIR names:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter UnlocksSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR.

private let unlockSnapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

private struct SampleUnlocks: UnlocksAPI {
    let now: Date

    func unlocks() async throws -> [OperatorUnlock] {
        [
            OperatorUnlock(id: "a", state: .pending, forSeconds: 900, pid: 48213,
                           command: "orch spaces unlock --for 15m -- orch rag relabel",
                           ancestry: ["/bin/zsh (48190)", "login (48189)", "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal (612)"],
                           requestedAt: now, expiresAt: now.addingTimeInterval(120),
                           executable: "/opt/homebrew/bin/orch", tty: "ttys003"),
            OperatorUnlock(id: "c", state: .confirmed, forSeconds: 600, pid: 48300, command: "orch spaces unlock --for 10m",
                           ancestry: ["/bin/zsh (48290)"], requestedAt: now, expiresAt: now.addingTimeInterval(110),
                           code: "482-913", executable: "/opt/homebrew/bin/orch", tty: "ttys005"),
            OperatorUnlock(id: "b", state: .active, forSeconds: 1800, pid: 47001, command: "orch spaces unlock --for 30m",
                           ancestry: ["/bin/zsh (46990)", "tmux (1203)"],
                           requestedAt: now.addingTimeInterval(-300), expiresAt: now.addingTimeInterval(1500)),
        ]
    }

    func confirmUnlock(id: String, allow: Bool) async throws {}
    func lockUnlock(id: String) async throws -> Int { 0 }
}

private struct NoConfirmer: UnlockConfirmer {
    func confirm(reason: String) async -> Bool { false }
}

@MainActor
@Suite(.enabled(if: unlockSnapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct UnlocksSnapshots {
    @Test func menuBarUnlocks() async throws {
        let dir = URL(fileURLWithPath: try #require(unlockSnapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let model = UnlocksModel(api: SampleUnlocks(now: Date()), confirmer: NoConfirmer())
        await model.load()
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            try render(
                UnlocksSection(model: model)
                    .padding(12).frame(width: 320, height: 520, alignment: .top)
                    .background(Color(nsColor: .windowBackgroundColor)),
                size: NSSize(width: 320, height: 520), appearance: appearance,
                to: dir.appendingPathComponent("menu-bar-unlocks-\(name).png")
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
