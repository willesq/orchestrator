import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Spaces section, the "enforcement off" banner and the menu bar
// panel's switches, in light and dark, from fixed sample data and without orchd, to the folder
// ORCH_SNAPSHOT_DIR names:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter OrchAppSnapshots
//
// The views are drawn offscreen (cacheDisplay), so no window shows and no
// Screen Recording permission is needed. Without ORCH_SNAPSHOT_DIR the
// suite is skipped, so `make app-test` and CI never write files.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

/// Sample switches: the guard off for 15 minutes, outbound terms off
/// untimed, the rest at their defaults.
private struct SampleSpaces: SpacesAPI {
    let now: Date

    func enforcement() async throws -> EnforcementState {
        EnforcementState(switches: [
            EnforcementSwitch(layer: "memory", title: "memory filtering", state: "on", defaultState: "on", states: ["on", "off"]),
            EnforcementSwitch(layer: "guard", title: "tool guard", state: "off", defaultState: "block-other-jobs",
                              states: ["off", "log", "block-other-jobs", "strict"], relaxed: true,
                              setAt: now.addingTimeInterval(-60), setBy: "operator (process 4242)",
                              until: now.addingTimeInterval(14 * 60), revertsTo: "block-other-jobs"),
            EnforcementSwitch(layer: "terms", title: "outbound terms", state: "off", defaultState: "on", states: ["on", "off"],
                              relaxed: true, setAt: now.addingTimeInterval(-3600), setBy: "operator (process 4242)"),
            EnforcementSwitch(layer: "sandbox", title: "sandbox rules", state: "on", defaultState: "on", states: ["on", "off"]),
            EnforcementSwitch(layer: "hooks", title: "tool hooks (pre-tool-use, cwd-changed)", state: "on", defaultState: "on",
                              states: ["on", "off"]),
        ])
    }

    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }

    /// A checkout under the private root whose remote belongs to an
    /// isolated org: the remote is stricter and decides.
    func explainPlacement(target: String) async throws -> PlacementExplanation {
        PlacementExplanation(
            roots: [PlacementRoot(root: "/Users/me/code", space: "oss", kind: "public"),
                    PlacementRoot(root: "/Users/me/code/willesq", space: "personal", kind: "private")],
            remote: PlacementRemote(owner: "acme", repo: "app", visibility: "private", space: "acme", kind: "isolated",
                                    effect: "owner bound to acme (isolated)"),
            rule: "remote_stricter", reason: "its remote's owner is bound to acme, stricter than its root",
            outcome: "placed", space: "acme", sessionSpace: "acme", sessionReads: "acme"
        )
    }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesSnapshots {
    @Test func spacesSectionAndBanner() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = SampleSpaces(now: Date())
        let enforcement = EnforcementModel(api: api)
        await enforcement.load()
        let validator = PlacementValidator(api: api, home: "/Users/me")
        validator.path = "~/code/willesq/app"
        await validator.check()

        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            try render(
                SpacesSettingsView(enforcement: enforcement, validator: validator).frame(width: 640, height: 680),
                size: NSSize(width: 640, height: 680), appearance: appearance,
                to: dir.appendingPathComponent("spaces-section-\(name).png")
            )
            try render(
                EnforcementBanner(model: enforcement, show: {}).padding(8).frame(width: 640, alignment: .top)
                    .background(Color(nsColor: .windowBackgroundColor)),
                size: NSSize(width: 640, height: 72), appearance: appearance,
                to: dir.appendingPathComponent("enforcement-banner-\(name).png")
            )
            // The menu bar panel's part: the banner and the compact switches.
            try render(
                VStack(alignment: .leading, spacing: 10) {
                    EnforcementBanner(model: enforcement, show: nil)
                    Divider()
                    TraySpaces(switches: enforcement.state?.switches ?? [], model: enforcement, openSpaces: {})
                }
                .padding(12).frame(width: 320, height: 330, alignment: .top)
                .background(Color(nsColor: .windowBackgroundColor)),
                size: NSSize(width: 320, height: 330), appearance: appearance,
                to: dir.appendingPathComponent("menu-bar-spaces-\(name).png")
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
        // Let SwiftUI finish its first pass (tasks, timeline views).
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
