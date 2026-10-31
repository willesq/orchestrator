import CoreGraphics
import Testing
@testable import OrchKit

@Suite struct CompanionLayoutTests {
    /// A 1512×945 screen with a 33pt menu bar and no Dock.
    private let visible = CGRect(x: 0, y: 0, width: 1512, height: 912)

    @Test func compactBelowTheThresholdOnly() {
        #expect(CompanionLayout.isCompact(width: CompanionLayout.minimumSize.width))
        #expect(CompanionLayout.isCompact(width: CompanionLayout.compactWidth - 1))
        #expect(!CompanionLayout.isCompact(width: CompanionLayout.compactWidth))
        #expect(!CompanionLayout.isCompact(width: CompanionLayout.expandedSize.width))
    }

    @Test func minimumSizeIsCompactAndExpandedIsNot() {
        #expect(CompanionLayout.minimumSize.width <= CompanionLayout.dockedWidth)
        #expect(CompanionLayout.isCompact(width: CompanionLayout.dockedWidth))
    }

    @Test func dockedFrameIsAFullHeightColumnAtTheRightEdge() {
        let frame = CompanionLayout.dockedFrame(in: visible)
        #expect(frame == CGRect(x: 1512 - 380 - 8, y: 8, width: 380, height: 896))
        #expect(visible.contains(frame))
    }

    @Test func dockedFrameFollowsAnOffsetScreen() {
        // A second display to the left of the main one, with a Dock at its foot.
        let left = CGRect(x: -1920, y: 70, width: 1920, height: 1010)
        let frame = CompanionLayout.dockedFrame(in: left)
        #expect(frame.maxX == -8)
        #expect(frame.minY == 78)
        #expect(left.contains(frame))
    }

    @Test func dockedFrameNeverGoesNegativeOnATinyScreen() {
        let frame = CompanionLayout.dockedFrame(in: CGRect(x: 0, y: 0, width: 10, height: 10))
        #expect(frame.width == 0)
        #expect(frame.height == 0)
    }

    @Test func expandingAColumnAtTheRightEdgeGrowsLeftAndKeepsItsTop() {
        let docked = CompanionLayout.dockedFrame(in: visible)
        let frame = CompanionLayout.expandedFrame(from: docked, in: visible)
        #expect(frame.width == 980)
        #expect(frame.maxX == docked.maxX)
        #expect(frame.maxY == docked.maxY)
        #expect(frame.height == docked.height)
    }

    @Test func expandingAColumnAtTheLeftEdgeGrowsRight() {
        let column = CGRect(x: 0, y: 300, width: 400, height: 500)
        let frame = CompanionLayout.expandedFrame(from: column, in: visible)
        #expect(frame == CGRect(x: 0, y: 160, width: 980, height: 640))
    }

    @Test func expandingAWideWindowLeavesItAlone() {
        let wide = CGRect(x: 100, y: 100, width: 1200, height: 700)
        #expect(CompanionLayout.expandedFrame(from: wide, in: visible) == wide)
    }

    @Test func expandingKeepsTheWindowOnScreen() {
        // Near the bottom-right corner: growing would push it off both edges.
        let corner = CGRect(x: 1300, y: 0, width: 200, height: 300)
        let frame = CompanionLayout.expandedFrame(from: corner, in: visible)
        #expect(visible.contains(frame))
        #expect(frame.size == CGSize(width: 980, height: 640))
    }

    @Test func expandingNeverExceedsASmallScreen() {
        let small = CGRect(x: 0, y: 0, width: 800, height: 500)
        let frame = CompanionLayout.expandedFrame(from: CGRect(x: 10, y: 10, width: 300, height: 300), in: small)
        #expect(frame == small)
    }
}
