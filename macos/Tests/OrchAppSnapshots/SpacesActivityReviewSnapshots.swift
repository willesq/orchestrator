import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Spaces pane's Activity and Review pages (SP-054) in light and
// dark, at the main window's pane width and the 320 pt companion width,
// from fixed sample data and without orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesActivityReviewSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]
private let home = "/Users/me"

private let appliedJSON = """
{"spaces":[
 {"name":"oss","kind":"public","github_orgs":["oss-org"]},
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["oss"]},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
 {"name":"personal","kind":"private","roots":["~/code"]}
]}
"""

/// The design's draft: acme also reads globex (refused) and personal gains
/// ~/notes, saved by an agent session.
private let draftJSON = """
{"spaces":[
 {"name":"oss","kind":"public","github_orgs":["oss-org"]},
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["oss","globex"]},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
 {"name":"personal","kind":"private","roots":["~/code","~/notes"]}
]}
"""

private struct SampleSpaces: SpacesReviewAPI, SpacesActivityAPI {
    let now: Date
    var draft = draftJSON
    var problems = [SpacesProblemInfo(rule: "isolated_reads_isolated", space: "acme",
                                      message: #"isolated space "acme" cannot read isolated space "globex"; merge them instead"#)]

    func diffSpaces() async throws -> SpacesPolicyDiff {
        SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON, draftRevision: 8, draftJSON: draft, problems: problems,
                         draftCreatedBy: "claude session 4f2a1b2c", draftCreatedAt: now.addingTimeInterval(-300),
                         draftByOperator: false, unconfirmedGitHubOrgs: ["globex"])
    }

    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft { SavedSpacesDraft(revision: 9) }

    func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64 {
        throw CancellationError()
    }

    func listSpaces() async throws -> SpacesListing {
        SpacesListing(status: SpacesPolicyStatus(configured: true, appliedRevision: 7, draftRevision: 8, drift: ["draft"]))
    }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        [
            PinnedSession(harness: "codex", sessionID: "9b11e7a0c4", harnessPID: 4102, space: "acme",
                          firstCwd: "\(home)/work/acme/web", state: "active", lastActivity: now.addingTimeInterval(-60),
                          toolEvents: 6, refusals: 2),
            PinnedSession(harness: "claude", sessionID: "4f2a1b2c9d", harnessPID: 4101, space: "acme",
                          firstCwd: "\(home)/work/acme/api", state: "idle", lastActivity: now.addingTimeInterval(-3600),
                          toolEvents: 214),
            PinnedSession(harness: "claude", sessionID: "77c0d31f2e", harnessPID: 4103, space: "globex",
                          firstCwd: "\(home)/work/globex/core", state: "ended", lastActivity: now.addingTimeInterval(-86400),
                          toolEvents: 41),
        ]
    }

    func toolEvents(owner: String, afterSeq: Int64, limit: Int32) async throws -> ToolLogPage {
        guard afterSeq == 0 else { return ToolLogPage(events: []) }
        let t = now.addingTimeInterval(-600)
        let cwd = "\(home)/work/acme/web"
        return ToolLogPage(events: [
            ToolLogEvent(seq: 1, at: t, tool: "Bash", cwd: cwd, command: "npm test", decision: "logged"),
            ToolLogEvent(seq: 2, at: t.addingTimeInterval(89), tool: "Read", cwd: cwd,
                         targets: #"{"paths":["~/work/acme/api/openapi.yaml"]}"#, decision: "logged"),
            ToolLogEvent(seq: 3, at: t.addingTimeInterval(171), tool: "Bash", cwd: cwd, command: "cd ~/work/globex/core && make",
                         decision: "refused", rule: "other_isolated_root"),
            ToolLogEvent(seq: 4, at: t.addingTimeInterval(178), tool: "Bash", agentType: "Explore", cwd: cwd,
                         command: "gh api repos/globex/core", decision: "refused", rule: "other_job_org"),
            ToolLogEvent(seq: 5, at: t.addingTimeInterval(240), event: "cwd-changed", cwd: cwd, oldCwd: cwd,
                         newCwd: "/tmp/build", decision: "logged"),
            ToolLogEvent(seq: 6, at: t.addingTimeInterval(259), tool: "Bash", cwd: "/tmp/build", command: "make",
                         decision: "logged"),
            ToolLogEvent(seq: 7, at: t.addingTimeInterval(300), kind: "gap", event: "", dropped: 2),
        ])
    }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesActivityReviewSnapshots {
    @Test func activityAndReview() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = SampleSpaces(now: Date())
        let activity = SpacesActivityModel(api: api, home: home)
        await activity.load()
        let review = SpacesReviewModel(api: api, authenticator: NeverAuthenticates(), home: home)
        await review.load()
        var clean = api
        clean.draft = try #require(SpacesReview.excluding(try await api.diffSpaces(), home: home))
        clean.problems = []
        let cleaned = SpacesReviewModel(api: clean, authenticator: NeverAuthenticates(), home: home)
        await cleaned.load()
        cleaned.acceptAgentDraft = true

        let sizes: [(String, NSSize)] = [("default", NSSize(width: 790, height: 980)), ("companion", NSSize(width: 320, height: 980))]
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            for (width, size) in sizes {
                try render(SpacesActivityView(model: activity), size: size, appearance: appearance,
                           to: dir.appendingPathComponent("spaces-activity-\(width)-\(name).png"))
                try render(SpacesReviewView(model: review), size: size, appearance: appearance,
                           to: dir.appendingPathComponent("spaces-review-\(width)-\(name).png"))
            }
            try render(SpacesReviewView(model: cleaned), size: sizes[0].1, appearance: appearance,
                       to: dir.appendingPathComponent("spaces-review-clean-default-\(name).png"))
        }
    }

    /// Draws `view` in an offscreen window and writes it as a PNG.
    private func render(_ view: some View, size: NSSize, appearance: NSAppearance.Name, to url: URL) throws {
        let hosting = NSHostingView(rootView: view.frame(width: size.width, height: size.height)
            .background(Color(nsColor: .windowBackgroundColor)))
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

private struct NeverAuthenticates: OperatorAuthenticator {
    func confirm(reason: String) async throws { throw CancellationError() }
}
