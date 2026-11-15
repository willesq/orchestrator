import OrchKit

// The ⌘K palette's Spaces section (SP-055): the tester, Review and each
// space, navigation only. MainWindow adds these to its commands.

extension PaletteCommand {
    /// The Spaces entries as palette commands; `open` moves the window to
    /// the entry's place in the Spaces pane.
    @MainActor
    static func spaces(_ spaces: SpacesOverviewModel, spacesOff: Bool,
                       open: @escaping (SpacesPaletteTarget) -> Void) -> [PaletteCommand] {
        SpacesPalette.entries(spaces: spaces.spaceChoices, spacesOff: spacesOff, shareSafe: spaces.shareSafe).map { entry in
            PaletteCommand(id: entry.id, section: "Spaces", title: entry.title, subtitle: entry.subtitle,
                           symbol: entry.symbol) { open(entry.target) }
        }
    }
}
