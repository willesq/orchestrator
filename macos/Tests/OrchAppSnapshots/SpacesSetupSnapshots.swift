import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the setup wizard (SP-052) at each step, and the Spaces pane that
// opens it, in light and dark, from the fixture machine and without orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesSetupSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]
private let home = "/Users/me"

/// The fixture machine: two job orgs, an open org with one private repo,
/// and the user's own account; nothing applied yet.
private final class SampleSetup: SpacesOverviewAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var saved: [String] = []

    func enforcement() async throws -> EnforcementState {
        EnforcementState(switches: [
            EnforcementSwitch(layer: "memory", title: "memory filtering", state: "on", defaultState: "on", states: ["off", "on"]),
            EnforcementSwitch(layer: "guard", title: "tool guard", state: "log", defaultState: "block-other-jobs",
                              states: ["off", "log", "block-other-jobs", "strict"], relaxed: true),
            EnforcementSwitch(layer: "terms", title: "outbound terms", state: "on", defaultState: "on", states: ["off", "on"]),
            EnforcementSwitch(layer: "sandbox", title: "sandbox rules", state: "on", defaultState: "on", states: ["off", "on"]),
        ])
    }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        EnforcementChangeResult(current: EnforcementSwitch(layer: layer, state: state))
    }
    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
    func spaceLogins() async throws -> [SpaceLogin] { [] }
    func listSpaces() async throws -> SpacesListing { SpacesListing() }
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] { [] }
    func toolLogSummary() async throws -> [SpaceActivity] { [] }
    func unconfirmedGitHubOrgs() async throws -> [String] { [] }

    func scanSpaces(folders: [String], offline: Bool) async throws -> SpacesScan {
        SpacesScan(
            checkouts: [
                ScannedCheckout(path: "\(home)/work/acme/api", owner: "acme", repo: "api", visibility: "private"),
                ScannedCheckout(path: "\(home)/work/acme/web", owner: "acme", repo: "web", visibility: "private"),
                ScannedCheckout(path: "\(home)/work/globex/core", owner: "globex", repo: "core", visibility: "private"),
                ScannedCheckout(path: "\(home)/code/oss-tools", owner: "oss-org", repo: "oss-tools", visibility: "public"),
                ScannedCheckout(path: "\(home)/code/oss-lib", owner: "oss-org", repo: "oss-lib", visibility: "public"),
                ScannedCheckout(path: "\(home)/code/oss-secret", owner: "oss-org", repo: "oss-secret", visibility: "private"),
                ScannedCheckout(path: "\(home)/code/site", owner: "me", repo: "site", visibility: "public"),
                ScannedCheckout(path: "\(home)/code/scratch"),
            ],
            accounts: [ScannedAccount(login: "me", orgs: ["acme", "globex", "oss-org"])]
        )
    }
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        lock.withLock { saved.append(policyJSON) }
        return SavedSpacesDraft(revision: Int64(lock.withLock { saved.count }))
    }
    func diffSpaces() async throws -> SpacesPolicyDiff {
        let n = Int64(lock.withLock { saved.count })
        return SpacesPolicyDiff(draftRevision: n, changes: n == 0 ? [] : [
            SpacesPolicyChange(space: "acme", kind: "added", details: ["kind: isolated", "github_orgs: acme", "roots: ~/work/acme"]),
            SpacesPolicyChange(space: "globex", kind: "added", details: ["kind: isolated", "github_orgs: globex"]),
            SpacesPolicyChange(space: "personal", kind: "added", details: ["kind: private", "github_orgs: me"]),
            SpacesPolicyChange(space: "public", kind: "added", details: ["kind: public", "github_orgs: oss-org"]),
        ])
    }
    func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64 {
        reviewedRevision
    }
    func commandLogRetention() async throws -> CommandLogRetention { CommandLogRetention(value: "7d") }
    func setCommandLogRetention(_ value: String) async throws {}
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesSetupSnapshots {
    @Test func wizard() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = SampleSetup()
        let model = SpacesSetupModel(api: api, home: home, folders: ["\(home)/work", "\(home)/code"], authorize: { _ in })
        await model.load()
        let size = NSSize(width: 760, height: 900)
        for step in SpacesSetupModel.Step.allCases {
            while model.step < step { await model.next() }
            try renderBoth(SpacesSetupView(model: model, finish: { _ in }), size: size, name: "spaces-setup-\(step.rawValue + 1)-\(step.title.lowercased())", dir: dir)
        }
        await model.apply()
        try renderBoth(SpacesSetupView(model: model, finish: { _ in }), size: NSSize(width: 760, height: 520), name: "spaces-setup-6-done", dir: dir)

        // The pane with nothing applied, whose button opens setup.
        let suite = "orch-spaces-setup-snapshots"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defaults.removePersistentDomain(forName: suite)
        let pane = SpacesOverviewModel(api: api, defaults: defaults, home: home)
        await pane.load()
        let enforcement = EnforcementModel(api: api)
        await enforcement.load()
        try renderBoth(SpacesView(model: pane, enforcement: enforcement, navigator: Navigator()), size: NSSize(width: 790, height: 520),
                       name: "spaces-pane-not-set-up", dir: dir)
        defaults.removePersistentDomain(forName: suite)
    }

    private func renderBoth(_ view: some View, size: NSSize, name: String, dir: URL) throws {
        for (look, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            try render(view.frame(width: size.width, height: size.height).background(Color(nsColor: .windowBackgroundColor)),
                       size: size, appearance: appearance, to: dir.appendingPathComponent("\(name)-\(look).png"))
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
        RunLoop.main.run(until: Date().addingTimeInterval(0.8))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
