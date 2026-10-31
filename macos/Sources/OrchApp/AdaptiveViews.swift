import OrchKit
import SwiftUI

/// Lays its subviews out in a row and wraps onto the next line instead of
/// running past the pane's edge, for rows of filters and chips.
struct FlowLayout: Layout {
    var spacing: CGFloat = 8
    var lineSpacing: CGFloat = 6

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let maxWidth = proposal.width ?? .infinity
        let sizes = subviews.map { $0.sizeThatFits(.unspecified) }
        let rows = AdaptiveLayout.rows(widths: sizes.map(\.width), spacing: spacing, maxWidth: maxWidth)
        var width: CGFloat = 0
        var height: CGFloat = 0
        for row in rows {
            let rowWidth = sizes[row].map(\.width).reduce(0, +) + spacing * CGFloat(row.count - 1)
            width = max(width, min(rowWidth, maxWidth))
            height += (sizes[row].map(\.height).max() ?? 0)
        }
        height += lineSpacing * CGFloat(max(rows.count - 1, 0))
        return CGSize(width: width, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let sizes = subviews.map { $0.sizeThatFits(.unspecified) }
        let rows = AdaptiveLayout.rows(widths: sizes.map(\.width), spacing: spacing, maxWidth: bounds.width)
        var y = bounds.minY
        for row in rows {
            let rowHeight = sizes[row].map(\.height).max() ?? 0
            var x = bounds.minX
            for index in row {
                // An item wider than the pane is narrowed to it, so it truncates
                // rather than spilling past the edge.
                let width = min(sizes[index].width, bounds.maxX - x)
                subviews[index].place(at: CGPoint(x: x, y: y + rowHeight / 2), anchor: .leading,
                                      proposal: ProposedViewSize(width: width, height: sizes[index].height))
                x += width + spacing
            }
            y += rowHeight + lineSpacing
        }
    }
}

/// Two columns side by side when both fit the pane, otherwise one at a time,
/// so neither is cut off by the window's edge. The pane reports no minimum
/// width of its own: it adapts to the window instead of overflowing it.
struct AdaptiveSplit<Primary: View, Secondary: View>: View {
    /// What the pane shows when the columns do not fit side by side.
    enum Stacking {
        /// A list and its detail: the detail replaces the list while `shown`,
        /// under a bar that goes back to the list.
        case detail(shown: Bool, backTitle: String, back: () -> Void)
        /// Two peers: a segmented control picks one.
        case tabs(primary: String, secondary: String)
    }

    struct Width {
        var min: CGFloat
        var ideal: CGFloat?
        var max: CGFloat = .infinity
    }

    let primaryWidth: Width
    let secondaryWidth: Width
    let stacking: Stacking
    /// Whether the secondary column shows side by side; a board opens its
    /// detail only for a selection.
    let secondaryPresent: Bool
    let primary: () -> Primary
    let secondary: () -> Secondary

    init(primaryWidth: Width, secondaryWidth: Width, stacking: Stacking, secondaryPresent: Bool = true,
         @ViewBuilder primary: @escaping () -> Primary, @ViewBuilder secondary: @escaping () -> Secondary) {
        self.primaryWidth = primaryWidth
        self.secondaryWidth = secondaryWidth
        self.stacking = stacking
        self.secondaryPresent = secondaryPresent
        self.primary = primary
        self.secondary = secondary
    }

    @State private var sideBySide = true
    @State private var showsSecondaryTab = false

    var body: some View {
        // Plain values, so the geometry closure does not capture the generic view.
        let primaryMin = primaryWidth.min
        let secondaryMin = secondaryPresent ? secondaryWidth.min : -AdaptiveLayout.dividerWidth
        Group {
            if sideBySide {
                HSplitView {
                    primary()
                        .frame(minWidth: primaryWidth.min, idealWidth: primaryWidth.ideal, maxWidth: primaryWidth.max,
                               maxHeight: .infinity)
                    if secondaryPresent {
                        secondary()
                            .frame(minWidth: secondaryWidth.min, idealWidth: secondaryWidth.ideal,
                                   maxWidth: secondaryWidth.max, maxHeight: .infinity)
                    }
                }
            } else {
                stacked
            }
        }
        .frame(minWidth: 0, maxWidth: .infinity, minHeight: 0, maxHeight: .infinity, alignment: .topLeading)
        .onGeometryChange(for: Bool.self) { proxy in
            AdaptiveLayout.fitsSideBySide(width: proxy.size.width, primaryMin: primaryMin, secondaryMin: secondaryMin)
        } action: { sideBySide = $0 }
    }

    @ViewBuilder private var stacked: some View {
        switch stacking {
        case .detail(let shown, let backTitle, let back):
            if shown {
                VStack(spacing: 0) {
                    HStack {
                        Button(action: back) { Label(backTitle, systemImage: "chevron.left") }
                            .buttonStyle(.borderless)
                            .keyboardShortcut("[", modifiers: .command)
                            .help("Back to the list (⌘[)")
                        Spacer()
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    Divider()
                    secondary().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            } else {
                primary().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        case .tabs(let primaryTitle, let secondaryTitle):
            VStack(spacing: 0) {
                Picker("Show", selection: $showsSecondaryTab) {
                    Text(primaryTitle).tag(false)
                    Text(secondaryTitle).tag(true)
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .padding(8)
                Divider()
                Group {
                    if showsSecondaryTab { secondary() } else { primary() }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }
}
