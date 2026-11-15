import Testing
@testable import OrchKit

// The ⌘K palette's Spaces entries (SP-055): the tester, Review and each
// space, navigation only, and none while spaces are off or the screen is
// shared.

@Suite struct SpacesPaletteTests {
    private let spaces: [(id: String, name: String)] = [(id: "acme", name: "acme"), (id: "oss", name: "oss")]

    @Test func listsTheTesterReviewAndEachSpace() {
        let entries = SpacesPalette.entries(spaces: spaces, spacesOff: false, shareSafe: nil)
        #expect(entries.map(\.title) == ["Where would this go?…", "Review space changes", "Space: acme", "Space: oss"])
        #expect(entries.map(\.target) == [.tester, .review, .space("acme"), .space("oss")])
        #expect(Set(entries.map(\.id)).count == entries.count)
    }

    @Test func offersTheTesterAndReviewWithNoSpaceApplied() {
        let entries = SpacesPalette.entries(spaces: [], spacesOff: false, shareSafe: nil)
        #expect(entries.map(\.target) == [.tester, .review])
    }

    @Test func listsNothingWhileSpacesAreOff() {
        #expect(SpacesPalette.entries(spaces: spaces, spacesOff: true, shareSafe: nil).isEmpty)
    }

    @Test func listsNothingWhileTheScreenIsShared() {
        // Screen-share-safe mode must not name the other spaces.
        #expect(SpacesPalette.entries(spaces: spaces, spacesOff: false, shareSafe: "acme").isEmpty)
    }

    @Test func showsTheSafeNameAndTargetsOrchdsName() {
        let entries = SpacesPalette.entries(spaces: [(id: "a\u{202E}b", name: "a b")], spacesOff: false, shareSafe: nil)
        #expect(entries.last?.title == "Space: a b")
        #expect(entries.last?.target == .space("a\u{202E}b"))
    }
}
