import AppKit
import Foundation
import GRPCCore
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Memory pane's Direct messages tab (SP-083) in light and dark,
// at the default pane width and the 320 pt companion width: before a search,
// with results (one stored with spaces off), refused, failed, and in
// screen-share-safe mode. Fixed sample data, no orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter DirectMessagesSnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

private struct Canned: DirectMessagesAPI {
    let answer: Result<DirectMessageResult, RPCError>

    func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult { try answer.get() }
}

private let sample = DirectMessageResult(hits: [
    DirectMessageHit(index: 0, text: "Can you review my patch before the release train on Thursday? The checklist is in the wiki.",
                     sourceURI: "slack://TACME/D0001", tenant: "slack:TACME", space: "acme", score: 0.82,
                     startedAt: Date(timeIntervalSince1970: 1_793_000_000)),
    DirectMessageHit(index: 1, text: "Standup moved to 10:30; I will send the notes after.",
                     sourceURI: "teams://globex-tenant/19:dm@unq.gbl.spaces", tenant: "teams:globex-tenant", space: "globex",
                     score: 0.61, startedAt: Date(timeIntervalSince1970: 1_792_500_000)),
    DirectMessageHit(index: 2, text: "Thanks, merged.", sourceURI: "slack://TOTHER/D0042", tenant: "slack:TOTHER", space: "",
                     score: 0.40),
])

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct DirectMessagesSnapshots {
    @Test func directMessages() async throws {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        let states: [(String, Result<DirectMessageResult, RPCError>, Bool, Bool)] = [
            ("empty", .success(sample), false, false),
            ("results", .success(sample), true, false),
            ("refused", .failure(RPCError(code: .permissionDenied, message: "only the operator can search direct messages")), true, false),
            ("failed", .failure(RPCError(code: .unavailable, message: "connection refused")), true, false),
            ("sharing", .success(sample), true, true),
        ]
        for (state, answer, search, sharing) in states {
            let model = DirectMessageSearchModel(api: Canned(answer: answer), isSharing: { sharing })
            model.query.text = search ? "release train" : ""
            if search { await model.search() }
            try await renderAll(state, model: model, dir: dir)
        }
    }

    private func renderAll(_ state: String, model: DirectMessageSearchModel, dir: URL) async throws {
        let sizes: [(String, NSSize)] = [("default", NSSize(width: 790, height: 520)), ("companion", NSSize(width: 320, height: 640))]
        for (name, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            for (width, size) in sizes {
                try await render(DirectMessageSearchView(model: model)
                    .frame(width: size.width, height: size.height)
                    .background(Color(nsColor: .windowBackgroundColor)),
                    size: size, appearance: appearance,
                    to: dir.appendingPathComponent("dm-search-\(state)-\(width)-\(name).png"))
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
        try await Task.sleep(for: .milliseconds(500))
        hosting.layoutSubtreeIfNeeded()
        let rep = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        let png = try #require(rep.representation(using: .png, properties: [:]))
        try png.write(to: url)
        print("SNAPSHOT \(url.path)")
    }
}
