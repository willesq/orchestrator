import Testing
@testable import OrchKit

// The command palette's fuzzy matching: what matches, and in what order.

@Suite struct PaletteMatcherTests {
    @Test func blankQueryMatchesEverything() {
        #expect(PaletteMatcher.score("", in: "Dashboard") == 0)
        #expect(PaletteMatcher.score("   ", in: "") == 0)
    }

    @Test(arguments: [
        ("dash", "Dashboard"),
        ("DASH", "dashboard"),
        ("nt", "New task"),
        ("tf31", "willesq/terraform#31"),
        ("safety net", "Safety Net"),
    ])
    func matchesInOrderIgnoringCase(query: String, text: String) {
        #expect(PaletteMatcher.score(query, in: text) != nil)
    }

    @Test(arguments: [
        ("xyz", "Dashboard"),
        ("draob", "Dashboard"),
        ("dashboards", "Dashboard"),
        ("a", ""),
    ])
    func rejectsWhatIsNotASubsequence(query: String, text: String) {
        #expect(PaletteMatcher.score(query, in: text) == nil)
    }

    @Test func prefersPrefixWordStartsAndRuns() throws {
        let prefix = try #require(PaletteMatcher.score("ta", in: "Tasks"))
        let wordStart = try #require(PaletteMatcher.score("ta", in: "New task"))
        let scattered = try #require(PaletteMatcher.score("ta", in: "Sentinel ward"))
        #expect(prefix > wordStart)
        #expect(wordStart > scattered)
    }

    @Test func rankOrdersByScoreThenGivenOrder() {
        let items = ["Sentinel", "New task", "Tasks", "Docs", "Another task"]
        #expect(PaletteMatcher.rank(items, query: "task") { $0 } == ["Tasks", "New task", "Another task"])
        #expect(PaletteMatcher.rank(items, query: "") { $0 } == items)
        #expect(PaletteMatcher.rank(items, query: "", limit: 2) { $0 } == ["Sentinel", "New task"])
        #expect(PaletteMatcher.rank(items, query: "zzz") { $0 }.isEmpty)
    }

    @Test func groupedKeepsFirstAppearanceOrder() {
        let ranked = [("PR", 1), ("Needs", 2), ("PR", 3), ("Go", 4), ("Needs", 5)]
        let grouped = PaletteMatcher.grouped(ranked) { $0.0 }.map(\.1)
        #expect(grouped == [1, 3, 2, 5, 4])
        #expect(PaletteMatcher.grouped([String]()) { $0 }.isEmpty)
    }
}
