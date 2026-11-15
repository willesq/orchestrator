import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Spaces pane's Overview (SP-051) in light and dark, at the main
// window's default pane width and at the 320 pt companion width, plus the
// screen-share-safe Overview, from fixed sample data and without orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesPaneSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]
private let home = "/Users/me"

/// The design's sample: two jobs, a personal space and a public one, with a
/// conflict, an unconfirmed org, a pending draft and the guard off.
private struct SamplePane: SpacesOverviewAPI {
    let now: Date

    func enforcement() async throws -> EnforcementState {
        EnforcementState(switches: [
            EnforcementSwitch(layer: "guard", title: "tool guard", state: "log", defaultState: "block-other-jobs",
                              states: ["off", "log", "block-other-jobs", "strict"], relaxed: true,
                              setAt: now.addingTimeInterval(-480), setBy: "operator (process 4242)",
                              until: now.addingTimeInterval(52 * 60), revertsTo: "block-other-jobs"),
        ])
    }

    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }

    func explainPlacement(target: String) async throws -> PlacementExplanation {
        PlacementExplanation(rule: "conflict", outcome: "conflict", space: "quarantine")
    }

    func listSpaces() async throws -> SpacesListing {
        SpacesListing(
            status: SpacesPolicyStatus(configured: true, appliedRevision: 7, draftRevision: 8, drift: ["draft"]),
            spaces: [
                SpaceInfo(name: "oss", kind: "public", githubOrgs: ["oss-org"], webPrefixes: ["https://go.dev/"]),
                SpaceInfo(name: "acme", kind: "isolated", githubOrgs: ["acme"], roots: ["\(home)/work/acme"],
                          chat: ["slack:T0ACME"], ghConfigDir: "\(home)/.config/gh-acme"),
                SpaceInfo(name: "globex", kind: "isolated", githubOrgs: ["globex"], roots: ["\(home)/work/globex"],
                          chat: ["teams:globex"]),
                SpaceInfo(name: "personal", kind: "private", githubOrgs: ["willesq"], roots: ["\(home)/code"],
                          reads: []),
            ]
        )
    }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        [
            PinnedSession(harness: "claude", sessionID: "4f2a1b2c9d", harnessPID: 4101, space: "acme",
                          firstCwd: "\(home)/work/acme/api", state: "active", lastActivity: now.addingTimeInterval(-120),
                          toolEvents: 214, refusals: 0),
            PinnedSession(harness: "codex", sessionID: "9b11e7a0c4", harnessPID: 4102, space: "acme",
                          firstCwd: "\(home)/work/acme/web", state: "active", lastActivity: now.addingTimeInterval(-600),
                          toolEvents: 88, refusals: 1),
            PinnedSession(harness: "claude", sessionID: "77c0d31f2e", harnessPID: 4103, space: "globex",
                          firstCwd: "\(home)/work/globex/core", state: "idle",
                          lastActivity: now.addingTimeInterval(-3 * 86400), toolEvents: 41),
            PinnedSession(harness: "claude", sessionID: "c41e0f88aa", harnessPID: 4104, space: "quarantine",
                          firstCwd: "\(home)/work/acme/billing", state: "active", lastActivity: now.addingTimeInterval(-30),
                          toolEvents: 3),
            PinnedSession(harness: "codex", harnessPID: 4120, state: "unpinned"),
        ]
    }

    func toolLogSummary() async throws -> [SpaceActivity] {
        [
            SpaceActivity(space: "acme", sessions: 2, toolCalls: 302, directoryMoves: 4, crossSpaceMoves: 1, refusals: 1),
            SpaceActivity(space: "globex", sessions: 1, toolCalls: 41, directoryMoves: 0),
        ]
    }

    func unconfirmedGitHubOrgs() async throws -> [String] { [] }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesPaneSnapshots {
    @Test func overview() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = SamplePane(now: Date())
        let enforcement = EnforcementModel(api: api)
        await enforcement.load()
        let suite = "orch-spaces-pane-snapshots"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defaults.removePersistentDomain(forName: suite)
        let model = SpacesOverviewModel(api: api, defaults: defaults, home: home)
        await model.load()
        let navigator = Navigator()

        // 790 pt is the detail column of the 980 pt default window; 320 pt
        // is the companion column.
        let sizes: [(String, NSSize)] = [("default", NSSize(width: 790, height: 640)), ("companion", NSSize(width: 320, height: 640))]
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            for (width, size) in sizes {
                try render(SpacesView(model: model, enforcement: enforcement, navigator: navigator)
                    .frame(width: size.width, height: size.height)
                    .background(Color(nsColor: .windowBackgroundColor)),
                    size: size, appearance: appearance,
                    to: dir.appendingPathComponent("spaces-pane-\(width)-\(name).png"))
            }
        }
        await model.shareOnly("acme")
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            let size = NSSize(width: 790, height: 640)
            try render(SpacesView(model: model, enforcement: enforcement, navigator: navigator)
                .frame(width: size.width, height: size.height)
                .background(Color(nsColor: .windowBackgroundColor)),
                size: size, appearance: appearance,
                to: dir.appendingPathComponent("spaces-pane-share-safe-\(name).png"))
        }
        defaults.removePersistentDomain(forName: suite)
    }

    /// Draws `view` in an offscreen window and writes it as a PNG.
    private func render(_ view: some View, size: NSSize, appearance: NSAppearance.Name, to url: URL) throws {
        let hosting = NSHostingView(rootView: view)
        hosting.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: appearance)
        window.contentView = hosting
        hosting.layoutSubtreeIfNeeded()
        // Let SwiftUI finish its first pass (tasks, geometry, timeline views).
        RunLoop.main.run(until: Date().addingTimeInterval(0.8))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
