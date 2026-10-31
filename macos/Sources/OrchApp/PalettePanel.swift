import AppKit
import SwiftUI

/// Hosts the command palette in its own floating panel over the main window.
/// An overlay inside the window shares its responder chain, so the pane's own
/// search field kept the keyboard; a panel of its own is key while it shows,
/// and closes as soon as the user clicks anywhere else.
@MainActor
final class PalettePanel: NSObject, NSWindowDelegate {
    static let shared = PalettePanel()

    private var panel: NSPanel?

    var isShown: Bool { panel?.isVisible ?? false }

    /// Shows the palette over `parent`, or closes it when it is already up.
    func toggle(over parent: NSWindow?, commands: [PaletteCommand]) {
        if isShown { close(); return }
        guard let parent else { return }
        let view = CommandPaletteView(commands: commands) { [weak self] in self?.close() }
        let hosting = NSHostingController(rootView: view)
        let panel = KeyPanel(contentViewController: hosting)
        panel.styleMask = [.borderless, .fullSizeContentView]
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.level = .floating
        panel.delegate = self
        let size = hosting.view.fittingSize
        let frame = parent.frame
        panel.setFrame(NSRect(x: frame.midX - size.width / 2, y: frame.maxY - 90 - size.height,
                              width: size.width, height: size.height), display: false)
        parent.addChildWindow(panel, ordered: .above)
        panel.makeKeyAndOrderFront(nil)
        self.panel = panel
    }

    func close() {
        guard let panel else { return }
        self.panel = nil
        panel.delegate = nil
        panel.parent?.removeChildWindow(panel)
        panel.orderOut(nil)
    }

    func windowDidResignKey(_ notification: Notification) { close() }
}

/// A borderless panel cannot become key by default, and the palette's text
/// field needs the keyboard.
private final class KeyPanel: NSPanel {
    override var canBecomeKey: Bool { true }
}
