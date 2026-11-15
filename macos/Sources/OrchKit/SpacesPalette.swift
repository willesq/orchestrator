/// Where a Spaces entry in the ⌘K palette goes (SP-055). Every target is a
/// place in the Spaces pane: the palette only navigates, so applying,
/// switching and retagging stay behind the pane's own confirmations.
public enum SpacesPaletteTarget: Sendable, Equatable {
    /// The Edit page's Rules editor, with its "Where would this go?" tester.
    case tester
    /// The Review page: the pending draft against the applied policy.
    case review
    /// The Overview, scrolled to one space's card (orchd's name).
    case space(String)
}

/// One Spaces entry in the ⌘K palette.
public struct SpacesPaletteEntry: Sendable, Equatable, Identifiable {
    public let id: String
    public let title: String
    public let subtitle: String?
    public let symbol: String
    public let target: SpacesPaletteTarget
}

public enum SpacesPalette {
    /// The palette's Spaces section: the tester, Review, then each space.
    /// Empty while spaces are off (the sidebar has no Spaces pane then) and
    /// in screen-share-safe mode, which must not list the other spaces.
    /// `spaces` are (id: orchd's name, name: safe to show) as the Overview
    /// lists them.
    public static func entries(spaces: [(id: String, name: String)], spacesOff: Bool,
                               shareSafe: String?) -> [SpacesPaletteEntry] {
        guard !spacesOff, shareSafe == nil else { return [] }
        var entries = [
            SpacesPaletteEntry(id: "spaces:tester", title: "Where would this go?…",
                               subtitle: "Check which space a folder, URL, repo or chat's content goes to",
                               symbol: "questionmark.folder", target: .tester),
            SpacesPaletteEntry(id: "spaces:review", title: "Review space changes",
                               subtitle: "The draft against the applied policy, before you apply it",
                               symbol: "doc.badge.gearshape", target: .review),
        ]
        for space in spaces {
            entries.append(SpacesPaletteEntry(id: "spaces:space:\(space.id)", title: "Space: \(space.name)",
                                              subtitle: "Show it in the Spaces pane", symbol: "square.3.layers.3d",
                                              target: .space(space.id)))
        }
        return entries
    }
}
