import Foundation
import Testing
@testable import OrchKit

/// A fixed clock and calendar, so ages and days are exact.
private let utc: Calendar = {
    var c = Calendar(identifier: .gregorian)
    c.timeZone = TimeZone(identifier: "UTC")!
    return c
}()

private let now = Date(timeIntervalSince1970: 1_794_830_400) // 2026-11-16 12:00:00 UTC

private func day(_ y: Int, _ m: Int, _ d: Int, _ h: Int = 0, _ min: Int = 0, _ s: Int = 0) -> Date {
    utc.date(from: DateComponents(year: y, month: m, day: d, hour: h, minute: min, second: s))!
}

private func parse(_ raw: String) -> ParsedMemoryQuery { MemoryFilterParser.parse(raw, now: now, calendar: utc) }

@Suite struct MemoryFilterParserTests {
    /// Text in, words left over and the filters read.
    struct Case: Sendable, CustomStringConvertible {
        let raw: String
        let text: String
        var type = ""
        var scope = ""
        var uri = ""
        var tags: [String] = []
        var collections: [String] = []
        var description: String { raw }
    }

    @Test(arguments: [
        Case(raw: "plain words", text: "plain words"),
        Case(raw: "type:repo_doc retry", text: "retry", type: "repo_doc"),
        Case(raw: "TYPE:web x", text: "x", type: "web"),
        Case(raw: "repo:revok readme", text: "readme", tags: ["repo:revok"]),
        Case(raw: "tag:owner:willesq tag:x", text: "", tags: ["owner:willesq", "x"]),
        Case(raw: "collection:a collection:b q", text: "q", collections: ["a", "b"]),
        Case(raw: "uri:session://abc/transcript q", text: "q", uri: "session://abc/transcript"),
        Case(raw: "uri:\"file:///a b.md\" q", text: "q", uri: "file:///a b.md"),
        Case(raw: "scope:o/r q", text: "q", scope: "o/r"),
        Case(raw: "\"exact phrase\" rest of words", text: "\"exact phrase\" rest of words"),
        Case(raw: "\"type:repo_doc in quotes\"", text: "\"type:repo_doc in quotes\""),
        Case(raw: "fix: bug https://example.com/x", text: "fix: bug https://example.com/x"),
        Case(raw: "  spaced   out  ", text: "spaced out"),
        Case(raw: "", text: ""),
    ])
    func readsFilters(_ c: Case) {
        let p = parse(c.raw)
        #expect(p.text == c.text)
        #expect(p.sourceType == c.type)
        #expect(p.scope == c.scope)
        #expect(p.sourceURI == c.uri)
        #expect(p.tags == c.tags)
        #expect(p.collections == c.collections)
        #expect(p.notes.isEmpty)
    }

    @Test(arguments: [
        ("since:7d", day(2026, 11, 9, 12)),
        ("since:24h", day(2026, 11, 15, 12)),
        ("since:2w", day(2026, 11, 2, 12)),
        ("since:today", day(2026, 11, 16)),
        ("since:yesterday", day(2026, 11, 15)),
        ("since:2026-11-01", day(2026, 11, 1)),
        ("since:2026-11-01T08:30:00Z", day(2026, 11, 1, 8, 30)),
    ])
    func sinceBounds(raw: String, expected: Date) {
        let p = parse(raw)
        #expect(p.since == expected && p.notes.isEmpty && p.chips.first?.key == .since)
    }

    @Test(arguments: [
        ("until:2026-11-01", day(2026, 11, 1, 23, 59, 59)),
        ("until:today", day(2026, 11, 16, 23, 59, 59)),
        ("until:yesterday", day(2026, 11, 15, 23, 59, 59)),
        ("until:1d", day(2026, 11, 15, 12)),
    ])
    func untilIsTheEndOfADay(raw: String, expected: Date) {
        #expect(parse(raw).until == expected)
    }

    @Test(arguments: ["since:abc", "since:2026-02-30", "until:7x", "since:-3d", "since:2026-1-1"])
    func badDatesAreNotedAndLeftOut(raw: String) {
        let p = parse("\(raw) words")
        #expect(p.text == "words" && p.since == nil && p.until == nil && p.chips.isEmpty)
        #expect(p.notes.count == 1 && p.notes[0].contains("is not a date"))
    }

    @Test func emptyValuesAreNoted() {
        let p = parse("type: words")
        #expect(p.text == "words" && p.sourceType.isEmpty && p.notes == ["type: needs a value, so it was left out"])
    }

    @Test func aLaterSingleFilterReplacesAnEarlierOne() {
        let p = parse("type:a type:b q")
        #expect(p.sourceType == "b")
        #expect(p.chips.map(\.value) == ["b"] && p.chips[0].id == 1)
        #expect(p.notes == ["only one type: filter applies, so b replaced a"])
    }

    @Test func sinceAfterUntilIsNoted() {
        #expect(parse("since:today until:yesterday").notes == ["since is after until, so nothing can match"])
    }

    @Test func chipsLabelAndRemoveTheirToken() {
        let raw = "retry repo:revok \"exact phrase\" since:7d"
        let p = parse(raw)
        #expect(p.chips.map(\.label) == ["Repo: revok", "Since: 7d"])
        #expect(MemoryFilterParser.removing(p.chips[0], from: raw) == "retry \"exact phrase\" since:7d")
        #expect(MemoryFilterParser.removing(p.chips[1], from: raw) == "retry repo:revok \"exact phrase\"")
        #expect(Set(MemoryFilterKey.allCases.map(\.label)).count == MemoryFilterKey.allCases.count)
    }

    @Test func tokensQuoteSpacesAndRefuseQuotes() {
        #expect(MemoryFilterParser.token(.uri, "repo://o/r") == "uri:repo://o/r")
        #expect(MemoryFilterParser.token(.uri, "a b") == "uri:\"a b\"")
        #expect(MemoryFilterParser.token(.uri, "a\"b") == nil)
        #expect(MemoryFilterParser.token(.uri, "") == nil)
    }

    @Test func appliedFiltersOverrideMenusAndAddTags() {
        let p = parse("q type:web uri:u collection:c scope:s repo:r since:today until:today")
        let base = SearchQuery(text: "ignored", sourceType: "repo_doc", tags: ["t"], collections: ["base"])
        let q = p.applied(to: base)
        #expect(q.text == "q" && q.sourceType == "web" && q.sourceURI == "u" && q.scope == "s")
        #expect(q.collections == ["c"] && q.tags == ["t"] && q.allTags == ["repo:r"])
        #expect(q.since == day(2026, 11, 16) && q.until == day(2026, 11, 16, 23, 59, 59))
        let b = p.applied(to: BrowseQuery(sourceType: "repo_doc", tags: ["t"]))
        #expect(b.sourceType == "web" && b.sourceURI == "u" && b.scope == "s" && b.collections == ["c"])
        #expect(b.tags == ["t"] && b.allTags == ["repo:r"] && b.since == day(2026, 11, 16))
        let untouched = parse("q").applied(to: base)
        #expect(untouched.sourceType == "repo_doc" && untouched.collections == ["base"] && untouched.since == nil)
    }
}

@Suite struct SearchHitDetailTests {
    @Test func repoDisambiguatesTitles() {
        let hit = SearchHit(text: "", sourceURI: "repo://o/revok/README.md", tags: ["owner:o", "repo:revok"])
        #expect(hit.repo == "revok")
        #expect(hit.displayTitle == "README.md · revok")
        #expect(SearchHit(text: "", sourceURI: "repo://o/revok/README.md").displayTitle == "README.md")
        let named = SearchHit(text: "", sourceURI: "https://example.com/revok", tags: ["repo:revok"])
        #expect(named.displayTitle == "revok", "a title naming its repo is not repeated")
        let tricky = SearchHit(text: "", sourceURI: "x://h/a.md", tags: ["repo:r\u{202E}x"])
        #expect(!tricky.displayTitle.contains("\u{202E}"))
    }

    @Test(arguments: [
        ("file:///tmp/notes.md", true, false),
        ("file://localhost/tmp/a.md", true, false),
        ("https://example.com/page", false, true),
        ("http://example.com", false, true),
        ("file://evil.example.com/share/a", false, false),
        ("file:relative/path", false, false),
        ("javascript:alert(1)", false, false),
        ("session://abc/transcript", false, false),
        ("https://user:pw@example.com/", false, false),
        ("", false, false),
    ])
    func onlyFileAndWebLinksOpen(uri: String, file: Bool, web: Bool) {
        let link = SearchHit(text: "", sourceURI: uri).sourceLink
        if case .file? = link { #expect(file) } else { #expect(!file) }
        if case .web? = link { #expect(web) } else { #expect(!web) }
    }

    @Test func jsonCarriesEveryField() throws {
        let hit = SearchHit(
            text: "body", sourceURI: "repo://o/r/a.md", sourceType: "repo_doc", scope: "o/r", score: 0.5, keywordScore: 2,
            exact: true, tags: ["repo:r"], startedAt: Date(timeIntervalSince1970: 0), endedAt: nil,
            ingestedAt: Date(timeIntervalSince1970: 60), heading: "Intro", trust: "repo", space: "work", tenant: "",
            collection: "base", pointID: "p-1", chunkIndex: 3
        )
        let object = try #require(try JSONSerialization.jsonObject(with: Data(hit.json.utf8)) as? [String: Any])
        #expect(Set(object.keys) == [
            "text", "source_uri", "source_type", "scope", "score", "keyword_score", "exact", "tags", "started_at",
            "ended_at", "ingested_at", "heading", "trust", "space", "tenant", "collection", "point_id", "chunk_index",
        ])
        #expect(object["started_at"] as? String == "1970-01-01T00:00:00Z")
        #expect(object["ended_at"] is NSNull)
        #expect(object["chunk_index"] as? Int == 3 && object["point_id"] as? String == "p-1")
        #expect(SearchHit(text: "", sourceURI: "u").json.contains("\"chunk_index\" : null"))
    }

    @Test func scoreHelpExplainsEachScore() {
        let keyword = SearchHit(text: "", sourceURI: "u", score: 0.5, keywordScore: 1.25, exact: true).scoreHelp
        #expect(keyword.contains("Relevance 0.50") && keyword.contains("Keyword 1.25") && keyword.contains("Exact"))
        let meaning = SearchHit(text: "", sourceURI: "u", score: 0.5).scoreHelp
        #expect(meaning.contains("found by meaning") && !meaning.contains("Exact"))
    }

    @Test func collectionLabelsNameTheSpaceWhenOn() {
        let c = CollectionInfo(name: "orch-work", space: "work", chunks: 1, hybrid: true)
        #expect(c.label(spacesOn: true) == "work: orch-work — 1 chunk")
        #expect(c.label(spacesOn: false) == "orch-work — 1 chunk")
        #expect(CollectionInfo(name: "base", chunks: 2).label(spacesOn: true) == "base — 2 chunks")
    }

    @Test func browseQueryNormalizes() {
        let q = BrowseQuery(collections: ["", "a"], sourceType: " t ", scope: " s ", sourceURI: " u ", tags: ["", "x"],
                            allTags: ["", "y"], pageSize: 999).normalized
        #expect(q == BrowseQuery(collections: ["a"], sourceType: "t", scope: "s", sourceURI: "u", tags: ["x"], allTags: ["y"],
                                 pageSize: 200))
        #expect(BrowseQuery(pageSize: 0).normalized.pageSize == 1)
        #expect(unixSeconds(nil) == 0 && unixSeconds(Date(timeIntervalSince1970: 12.7)) == 12)
    }
}
