import CoreGraphics
import Testing
@testable import OrchKit

@Suite struct AdaptiveLayoutTests {
    @Test func columnsFitOnlyWithRoomForBothAndTheDivider() {
        #expect(AdaptiveLayout.fitsSideBySide(width: 641, primaryMin: 280, secondaryMin: 360))
        #expect(!AdaptiveLayout.fitsSideBySide(width: 640, primaryMin: 280, secondaryMin: 360))
        #expect(AdaptiveLayout.fitsSideBySide(width: 401, primaryMin: 400, secondaryMin: 0))
        #expect(!AdaptiveLayout.fitsSideBySide(width: 0, primaryMin: 1, secondaryMin: 0))
    }

    @Test func everythingOnOneRowWhenItFits() {
        #expect(AdaptiveLayout.rows(widths: [100, 100, 100], spacing: 10, maxWidth: 1000) == [0..<3])
        // Exactly the row's width still fits.
        #expect(AdaptiveLayout.rows(widths: [100, 100, 100], spacing: 10, maxWidth: 320) == [0..<3])
    }

    @Test func wrapsWhereTheNextItemWouldPassTheEdge() {
        #expect(AdaptiveLayout.rows(widths: [100, 100, 100], spacing: 10, maxWidth: 319) == [0..<2, 2..<3])
        #expect(AdaptiveLayout.rows(widths: [150, 150, 150, 150], spacing: 8, maxWidth: 310) == [0..<2, 2..<4])
    }

    @Test func anItemWiderThanTheRowGetsItsOwnRow() {
        #expect(AdaptiveLayout.rows(widths: [50, 500, 50], spacing: 10, maxWidth: 200) == [0..<1, 1..<2, 2..<3])
        #expect(AdaptiveLayout.rows(widths: [500], spacing: 10, maxWidth: 200) == [0..<1])
    }

    @Test func unboundedWidthKeepsOneRow() {
        #expect(AdaptiveLayout.rows(widths: [400, 400, 400], spacing: 10, maxWidth: .infinity) == [0..<3])
    }

    @Test func noItemsNoRows() {
        #expect(AdaptiveLayout.rows(widths: [], spacing: 10, maxWidth: 100).isEmpty)
    }
}
