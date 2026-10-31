import CoreGraphics

/// The decisions that keep a pane inside the window at any width: whether two
/// columns fit side by side, and how a row of controls wraps.
public enum AdaptiveLayout {
    /// The split view's divider between two columns.
    public static let dividerWidth: CGFloat = 1

    /// Whether a column at least `primaryMin` wide fits beside one at least
    /// `secondaryMin` wide in `width`.
    public static func fitsSideBySide(width: CGFloat, primaryMin: CGFloat, secondaryMin: CGFloat) -> Bool {
        width >= primaryMin + dividerWidth + secondaryMin
    }

    /// Packs items of the given widths, in order, into rows no wider than
    /// `maxWidth`. An item wider than a row gets a row to itself, where the
    /// caller narrows it, rather than pushing the row past the edge.
    public static func rows(widths: [CGFloat], spacing: CGFloat, maxWidth: CGFloat) -> [Range<Int>] {
        var rows: [Range<Int>] = []
        var start = 0
        var used: CGFloat = 0
        for (index, width) in widths.enumerated() {
            if index > start, used + spacing + width > maxWidth {
                rows.append(start..<index)
                start = index
                used = width
            } else {
                used += (index > start ? spacing : 0) + width
            }
        }
        if start < widths.count { rows.append(start..<widths.count) }
        return rows
    }
}
