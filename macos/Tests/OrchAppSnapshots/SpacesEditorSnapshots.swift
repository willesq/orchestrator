import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Spaces pane's Edit page (SP-053), each editor in light and
// dark at the default pane width and the 320 pt companion width, plus the
// page while memory spaces are off and with a refused nesting. Fixed sample
// data, no orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter SpacesEditorSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]
private let home = "/Users/me"
/// Sent as a space's terms before rendering; never drawn (SP-053.1.a2).
private let sampleTerms = "ProjectNightjar\nre:acme-[0-9]+\n"

private let samplePolicy = """
{"spaces":[
 {"name":"oss","kind":"public","roots":["~/code"],"public_paths":["~/code/tools/**"],"web_prefixes":["https://docs.oss.dev/"],"github_orgs":["oss-org"]},
 {"name":"personal","kind":"private","roots":["~/code/willesq"],"github_orgs":["willesq"]},
 {"name":"acme","kind":"isolated","roots":["~/code/company1"],"github_orgs":["acme"],"chat":["slack:TACME"],"terms_file":"~/.config/orch/terms/acme.txt","gh_config_dir":"~/.config/gh-acme","reads":["personal"]}
],"public_root_sessions":"personal"}
"""

private final class SampleEditor: SpacesEditorAPI, @unchecked Sendable {
    var state = "on"

    func status() async throws -> DaemonStatus { DaemonStatus(version: "s", uptimeSeconds: 1, subsystems: [], spacesState: state) }
    func diffSpaces() async throws -> SpacesPolicyDiff { SpacesPolicyDiff(appliedRevision: 7, appliedJSON: samplePolicy) }
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft { SavedSpacesDraft(revision: 8) }
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        [PinnedSession(harness: "claude", sessionID: "4f2a1b2c9d", space: "acme", state: "active", toolEvents: 214, refusals: 2),
         PinnedSession(harness: "codex", sessionID: "77aa", space: "personal", state: "idle", toolEvents: 31),
         PinnedSession(harness: "gemini", state: "unpinned")]
    }
    func enforcement() async throws -> EnforcementState {
        EnforcementState(switches: [
            EnforcementSwitch(layer: "memory", title: "Memory filtering", state: "on", defaultState: "on", states: ["on", "off"]),
            EnforcementSwitch(layer: "guard", title: "Tool guard", state: "block-other-jobs", defaultState: "block-other-jobs",
                              states: ["off", "log", "block-other-jobs", "strict"]),
            EnforcementSwitch(layer: "hooks", title: "Tool hooks", state: "on", defaultState: "on", states: ["on", "off"]),
        ])
    }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult { throw CancellationError() }
    func explainPlacement(target: String) async throws -> PlacementExplanation {
        PlacementExplanation(targetKind: "path", roots: [PlacementRoot(root: "\(home)/code", space: "oss", kind: "public"),
                                                         PlacementRoot(root: "\(home)/code/company1", space: "acme", kind: "isolated")],
                             rule: "root", reason: "the deepest root holding it is bound to acme", outcome: "placed", space: "acme",
                             sessionSpace: "acme", sessionReads: "acme")
    }
    func listSources(type: String, limit: Int) async throws -> SourceList {
        SourceList(sources: [SourceSummary(uri: "repo://acme/app/README.md", sourceType: "repo", chunks: 14),
                             SourceSummary(uri: "https://docs.oss.dev/guide", sourceType: "web", chunks: 6),
                             SourceSummary(uri: "file://\(home)/code/company1/api/NOTES.md", sourceType: "file", chunks: 3)],
                   totalSources: 3, totalChunks: 23)
    }
    func spaceTerms() async throws -> [SpaceTermsInfo] {
        [SpaceTermsInfo(space: "acme", termsFile: "~/.config/orch/terms/acme.txt", exists: true, count: 12, fingerprint: "0a1b2c3d4e5f"),
         SpaceTermsInfo(space: "oss"), SpaceTermsInfo(space: "personal")]
    }
    func setSpaceTerms(space: String, terms: String) async throws -> SpaceTermsInfo {
        SpaceTermsInfo(space: space, termsFile: "~/.config/orch/terms/acme.txt", exists: true, count: 2, fingerprint: "9f8e7d6c5b4a")
    }
    func quarantinedSources() async throws -> [QuarantinedSource] { [QuarantinedSource(uri: "file:///Users/me/Downloads/spec.md", sourceType: "file", chunks: 9)] }
    func retagSource(uri: String, space: String, confirm: Bool, expectFrom: String) async throws -> RetagOutcome { RetagOutcome() }
    func spaceChunkCounts() async throws -> [SpaceChunkCount] {
        [SpaceChunkCount(space: "acme", chunks: 4210, stale: 12), SpaceChunkCount(space: "personal", chunks: 1890)]
    }
}

extension SampleEditor: SpacesAPI {
    func changeOutboundRule(action: String, id: String, kind: String, value: String) async throws -> OutboundRuleChangeResult { throw CancellationError() }
    func outboundRules() async throws -> OutboundRulesState { OutboundRulesState(rules: []) }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct SpacesEditorSnapshots {
    @Test func editors() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let api = SampleEditor()
        let enforcement = EnforcementModel(api: api)
        await enforcement.load()
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(await model.setTerms(space: "acme", text: sampleTerms))
        model.tester.input = "~/code/company1/api"
        await model.tester.check()
        let navigator = Navigator()

        for tab in SpacesEditorTab.allCases {
            model.tab = tab
            try await renderAll(tab.rawValue, model: model, enforcement: enforcement, navigator: navigator, dir: dir)
        }
        // A refused nesting: a public root inside the job's root.
        model.tab = .spaces
        model.setList("roots", of: 0, to: ["~/code", "~/code/company1/open"])
        try await renderAll("spaces-refused", model: model, enforcement: enforcement, navigator: navigator, dir: dir)
        model.revert()
        api.state = "off"
        await model.load()
        try await renderAll("off", model: model, enforcement: enforcement, navigator: navigator, dir: dir)

        // What the snapshots were drawn from holds no term (INV-07).
        var drawn = ""
        for child in Mirror(reflecting: model).children { dump(child.value, to: &drawn) }
        #expect(!drawn.contains("Nightjar") && !drawn.contains("acme-[0-9]"))
    }

    private func renderAll(_ state: String, model: SpacesEditorModel, enforcement: EnforcementModel, navigator: Navigator,
                           dir: URL) async throws {
        let sizes: [(String, NSSize)] = [("default", NSSize(width: 790, height: 900)), ("companion", NSSize(width: 320, height: 900))]
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            for (width, size) in sizes {
                try await render(SpacesEditorView(model: model, enforcement: enforcement, navigator: navigator, openReview: {})
                    .frame(width: size.width, height: size.height)
                    .background(Color(nsColor: .windowBackgroundColor)),
                    size: size, appearance: appearance,
                    to: dir.appendingPathComponent("spaces-edit-\(state)-\(width)-\(name).png"))
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
        try await Task.sleep(for: .milliseconds(600))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
