import AppKit
import Foundation
import OrchKit
import SwiftUI
import Testing
@testable import OrchApp

// Renders the Memory pane's filter bar (spaces on and off, with typed
// filters as chips and a note), a hit's inspector, and the Search tab with
// results in light and dark. Fixed sample data, no orchd:
//
//   ORCH_SNAPSHOT_DIR=/tmp/orch-shots swift test --package-path macos --filter MemorySnapshots
//
// Skipped without ORCH_SNAPSHOT_DIR, like the other snapshot suites.

private let snapshotDir = ProcessInfo.processInfo.environment["ORCH_SNAPSHOT_DIR"]

private let sampleHits = [
    SearchHit(text: "Run `make app-test` for the Swift tests and `make app-cover` for the per-file gate.\n\nThe gate excludes generated code.",
              sourceURI: "repo://example/orchestrator/README.md", sourceType: "repo_doc", scope: "example/orchestrator",
              score: 0.82, keywordScore: 3.4, exact: true, tags: ["owner:example", "repo:orchestrator"],
              startedAt: Date(timeIntervalSince1970: 1_793_000_000), ingestedAt: Date(timeIntervalSince1970: 1_794_000_000),
              heading: "Build › Tests", trust: "repo", space: "work", collection: "orch-work", pointID: "6f1c2a90-0d3e-4b8c",
              chunkIndex: 7),
    SearchHit(text: "A different README with the same file name.", sourceURI: "repo://example/revok/README.md",
              sourceType: "repo_doc", score: 0.61, tags: ["repo:revok"], ingestedAt: Date(timeIntervalSince1970: 1_793_500_000),
              collection: "orch-work", pointID: "7a", chunkIndex: 0),
    SearchHit(text: "We decided to retry twice before giving up.", sourceURI: "session://1a2b3c4d/transcript",
              sourceType: "session_transcript", score: 0.44, startedAt: Date(timeIntervalSince1970: 1_792_000_000),
              trust: "session", collection: "orch-work", pointID: "9c"),
]

private struct Sample: MemoryAPI {
    let spacesOn: Bool

    func search(_ query: SearchQuery) async throws -> SearchResult { SearchResult(hits: sampleHits) }
    func listSources(type: String, limit: Int, collections: [String]) async throws -> SourceList {
        SourceList(sources: [SourceSummary(uri: "a", sourceType: "repo_doc"), SourceSummary(uri: "b", sourceType: "web")],
                   totalSources: 2, totalChunks: 9)
    }
    func listCollections() async throws -> CollectionList {
        spacesOn
            ? CollectionList(collections: [CollectionInfo(name: "orch-base", chunks: 63_012, hybrid: true),
                                           CollectionInfo(name: "orch-work", space: "work", chunks: 1_204, hybrid: true)],
                             spacesOn: true)
            : CollectionList(collections: [CollectionInfo(name: "orch-base", chunks: 63_012, hybrid: true)], spacesOn: false)
    }
    func browseChunks(_ query: BrowseQuery, cursor: String) async throws -> BrowsePage { BrowsePage(hits: sampleHits) }
}

@MainActor
@Suite(.enabled(if: snapshotDir != nil, "set ORCH_SNAPSHOT_DIR to render snapshots"), .serialized)
struct MemorySnapshots {
    @Test func filterBar() async throws {
        let dir = try outputDir()
        for spacesOn in [true, false] {
            let api = Sample(spacesOn: spacesOn)
            let collections = CollectionsModel(api: api)
            await collections.refresh()
            let search = SearchModel(api: api, paged: true)
            search.query.text = "retry repo:orchestrator since:7d until:nope"
            await search.search()
            let bar = MemoryFilterBar(
                text: .constant(search.query.text), prompt: "Search memory", submitTitle: "Search", submit: {},
                collections: collections, collection: .constant(spacesOn ? "orch-work" : ""),
                sourceType: .constant("repo_doc"), types: ["repo_doc", "web"], mode: .constant(.configured),
                pageSize: .constant(20), pageSizes: SearchQuery.limitChoices, minRelevance: .constant(0),
                chips: search.parsed.chips, notes: search.parsed.notes, remove: { _ in }
            )
            .padding(12)
            try await renderBoth(bar, size: NSSize(width: 790, height: 150),
                                 name: "memory-filter-bar-\(spacesOn ? "spaces-on" : "spaces-off")", dir: dir)
        }
    }

    @Test func inspector() async throws {
        let dir = try outputDir()
        let actions = HitActions(openSource: { _ in }, searchWithin: { _ in }, browseWithin: { _ in })
        try await renderBoth(HitInspector(hit: sampleHits[0], actions: actions), size: NSSize(width: 420, height: 720),
                             name: "memory-inspector", dir: dir)
    }

    @Test func searchResults() async throws {
        let dir = try outputDir()
        let api = Sample(spacesOn: false)
        let search = SearchModel(api: api, paged: true)
        search.query = SearchQuery(text: "readme tests", limit: 5)
        await search.search()
        let sources = SourcesModel(api: api)
        await sources.refresh()
        let collections = CollectionsModel(api: api)
        await collections.refresh()
        try await renderBoth(SearchPane(model: search, sources: sources, collections: collections, actions: HitActions()),
                             size: NSSize(width: 790, height: 520), name: "memory-search-results", dir: dir)
    }

    private func outputDir() throws -> URL {
        let dir = URL(fileURLWithPath: try #require(snapshotDir))
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        _ = NSApplication.shared
        return dir
    }

    private func renderBoth(_ view: some View, size: NSSize, name: String, dir: URL) async throws {
        for (suffix, appearance) in [("light", NSAppearance.Name.aqua), ("dark", .darkAqua)] {
            try await render(view.frame(width: size.width, height: size.height)
                .background(Color(nsColor: .windowBackgroundColor)),
                size: size, appearance: appearance, to: dir.appendingPathComponent("\(name)-\(suffix).png"))
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
