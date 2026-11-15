import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Spaces pane's "Relabel now" control (SP-079) in light and dark,
// at the default pane width and the 320 pt companion width: with stale
// chunks, during a relabel, and after it. Fixed sample data, no orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesRelabelSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]
private let home = "/Users/me"

/// Two spaces and 7,317 chunks stored before the policy was applied. A run
/// waits until the test lets it finish.
private final class StalePane: SpacesOverviewAPI, SpacesRelabelAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var stale: Int64 = 7317
    private var waiting: CheckedContinuation<Void, Never>?

    func enforcement() async throws -> EnforcementState { EnforcementState() }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }

    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }

    func listSpaces() async throws -> SpacesListing {
        SpacesListing(
            status: SpacesPolicyStatus(configured: true, appliedRevision: 7),
            spaces: [
                SpaceInfo(name: "acme", kind: "isolated", githubOrgs: ["acme"], roots: ["\(home)/work/acme"]),
                SpaceInfo(name: "personal", kind: "private", githubOrgs: ["willesq"], roots: ["\(home)/code"]),
            ]
        )
    }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        [PinnedSession(harness: "claude", sessionID: "4f2a1b2c9d", harnessPID: 4101, space: "acme",
                       firstCwd: "\(home)/work/acme/api", state: "active", toolEvents: 214)]
    }

    func toolLogSummary() async throws -> [SpaceActivity] {
        [SpaceActivity(space: "acme", sessions: 1, toolCalls: 214, directoryMoves: 2)]
    }

    func unconfirmedGitHubOrgs() async throws -> [String] { [] }

    func relabel(apply: Bool) async throws -> RelabelRun {
        guard apply else { return lock.withLock { RelabelRun(revision: 7, total: 9120, stale: stale) } }
        await withCheckedContinuation { c in lock.withLock { waiting = c } }
        return lock.withLock {
            stale = 0
            return RelabelRun(revision: 7, total: 9120, stale: 7317, changed: 7317, moved: 6904, applied: true)
        }
    }

    var isHeld: Bool { lock.withLock { waiting != nil } }

    func finish() {
        let c = lock.withLock {
            let c = waiting
            waiting = nil
            return c
        }
        c?.resume()
    }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesRelabelSnapshots {
    @Test func relabelNow() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = StalePane()
        let enforcement = EnforcementModel(api: api)
        let suite = "orch-spaces-relabel-snapshots"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defaults.removePersistentDomain(forName: suite)
        let relabel = SpacesRelabelModel(api: api)
        let model = SpacesOverviewModel(api: api, defaults: defaults, home: home, relabel: { await relabel.progress })
        relabel.onChange = { await model.load() }
        await model.load()
        await relabel.check()
        let navigator = Navigator()

        try await renderAll("stale", model: model, enforcement: enforcement, relabel: relabel, navigator: navigator, dir: dir)
        let run = Task { await relabel.start() }
        while !api.isHeld { try await Task.sleep(for: .milliseconds(10)) }
        try await renderAll("running", model: model, enforcement: enforcement, relabel: relabel, navigator: navigator, dir: dir)
        api.finish()
        await run.value
        try await renderAll("done", model: model, enforcement: enforcement, relabel: relabel, navigator: navigator, dir: dir)
        defaults.removePersistentDomain(forName: suite)
    }

    private func renderAll(_ state: String, model: SpacesOverviewModel, enforcement: EnforcementModel,
                           relabel: SpacesRelabelModel, navigator: Navigator, dir: URL) async throws {
        let sizes: [(String, NSSize)] = [("default", NSSize(width: 790, height: 520)), ("companion", NSSize(width: 320, height: 640))]
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            for (width, size) in sizes {
                try await render(SpacesView(model: model, enforcement: enforcement, navigator: navigator)
                    .environment(relabel)
                    .frame(width: size.width, height: size.height)
                    .background(Color(nsColor: .windowBackgroundColor)),
                    size: size, appearance: appearance,
                    to: dir.appendingPathComponent("spaces-relabel-\(state)-\(width)-\(name).png"))
            }
        }
    }

    /// Draws `view` in an offscreen window and writes it as a PNG.
    private func render(_ view: some View, size: NSSize, appearance: NSAppearance.Name, to url: URL) async throws {
        let hosting = NSHostingView(rootView: view)
        hosting.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: appearance)
        window.contentView = hosting
        hosting.layoutSubtreeIfNeeded()
        // Let SwiftUI finish its first pass (tasks, geometry, timeline
        // views). Awaiting rather than running the run loop lets the pane's
        // count, which runs on the main actor, finish too.
        try await Task.sleep(for: .milliseconds(800))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
