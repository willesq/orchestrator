import CoreGraphics

/// The main window's geometry when it works as a companion: a narrow column
/// beside the terminal the agents run in, rather than a full window that has
/// to be switched to.
public enum CompanionLayout {
    /// Below this width the window shows one column instead of the sidebar
    /// and a pane, which would otherwise clip (window managers tile through
    /// Accessibility, which ignores the window's minimum size).
    public static let compactWidth: CGFloat = 600
    /// The smallest the window goes; the companion column still fits.
    public static let minimumSize = CGSize(width: 320, height: 420)
    /// How wide the docked column is.
    public static let dockedWidth: CGFloat = 380
    /// The smallest frame expanding gives, so the full layout has room.
    public static let expandedSize = CGSize(width: 980, height: 640)
    /// The gap between the docked column and the screen's edges.
    public static let margin: CGFloat = 8

    public static func isCompact(width: CGFloat) -> Bool { width < compactWidth }

    /// A full-height column at the right edge of `visible`, the screen's area
    /// inside the menu bar and Dock (AppKit coordinates, y up).
    public static func dockedFrame(in visible: CGRect) -> CGRect {
        let width = max(min(dockedWidth, visible.width - 2 * margin), 0)
        let height = max(visible.height - 2 * margin, 0)
        return CGRect(x: visible.maxX - width - margin, y: visible.minY + margin, width: width, height: height)
    }

    /// `frame` grown to at least `expandedSize` and kept inside `visible`. It
    /// grows toward the screen's centre, so a column at the right edge widens
    /// leftwards, and keeps its top edge where it was.
    public static func expandedFrame(from frame: CGRect, in visible: CGRect) -> CGRect {
        let width = min(max(frame.width, expandedSize.width), visible.width)
        let height = min(max(frame.height, expandedSize.height), visible.height)
        let x = frame.midX > visible.midX ? frame.maxX - width : frame.minX
        let y = frame.maxY - height
        return CGRect(x: clamp(x, visible.minX, visible.maxX - width),
                      y: clamp(y, visible.minY, visible.maxY - height),
                      width: width, height: height)
    }

    private static func clamp(_ value: CGFloat, _ low: CGFloat, _ high: CGFloat) -> CGFloat {
        min(max(value, low), high)
    }
}
