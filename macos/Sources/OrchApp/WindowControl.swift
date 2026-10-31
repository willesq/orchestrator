import AppKit
import Observation
import OrchKit

/// Moves the main window between its full layout and a companion column
/// docked at the screen's edge, and keeps it above other windows on request.
@MainActor
@Observable
final class WindowControl {
    /// The frame the window had before it docked; nil while not docked.
    private(set) var restoreFrame: CGRect?
    /// Whether the window floats above other apps' windows.
    private(set) var isPinned = false
    /// Set by the app delegate once it builds the window.
    @ObservationIgnored weak var window: NSWindow?

    var isDocked: Bool { restoreFrame != nil }

    func toggleDock() {
        if isDocked { expand() } else { dock() }
    }

    /// Narrows the window to a column at its screen's right edge, pinned so
    /// the terminal beside it does not cover it.
    func dock() {
        guard let window, !isDocked, let visible = (window.screen ?? NSScreen.main)?.visibleFrame else { return }
        restoreFrame = window.frame
        setPinned(true)
        window.setFrame(CompanionLayout.dockedFrame(in: visible), display: true, animate: true)
    }

    /// Back to the full layout: the frame the window had before docking, or a
    /// window the user narrowed widened enough for the sidebar and a pane.
    func expand() {
        guard let window, let visible = (window.screen ?? NSScreen.main)?.visibleFrame else { return }
        let from = restoreFrame ?? window.frame
        if isDocked {
            restoreFrame = nil
            setPinned(false)
        }
        window.setFrame(CompanionLayout.expandedFrame(from: from, in: visible), display: true, animate: true)
    }

    func togglePinned() { setPinned(!isPinned) }

    private func setPinned(_ pinned: Bool) {
        isPinned = pinned
        guard let window else { return }
        window.level = pinned ? .floating : .normal
        // A pinned window follows the user across Spaces, like the terminal
        // it sits beside would if it were full screen.
        if pinned {
            window.collectionBehavior.insert(.canJoinAllSpaces)
        } else {
            window.collectionBehavior.remove(.canJoinAllSpaces)
        }
    }
}
