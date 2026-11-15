import AppKit
import OrchKit
import SwiftUI

/// Launch handling SwiftUI's App does not cover: one copy at a time, and a
/// visible window whenever the user opens the app. It can live in the menu bar
/// alone, so without this opening it could show nothing, and its icon may sit
/// behind the notch.
///
/// The main window is a plain NSWindow hosting SwiftUI, because a SwiftUI
/// `Window` scene can only be opened from a live view, and the menu bar label
/// is rendered as an image, so it cannot do it on AppKit's behalf.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    /// Built on first use, so a copy that hands off never opens a connection.
    private(set) lazy var model = AppModel()
    private var window: NSWindow?
    private var lastRequest: Date?
    private var launchedAsLoginItem = false

    func applicationWillFinishLaunching(_ notification: Notification) {
        // The launch Apple event is only available this early.
        let event = NSAppleEventManager.shared().currentAppleEvent
        launchedAsLoginItem = event?.paramDescriptor(forKeyword: keyAEPropData)?.enumCodeValue == keyAELaunchedAsLogInItem
        NSApp.setActivationPolicy(DockIcon.policy(shown: DockIcon.shown()))
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Listen before deciding, so a copy that starts now and hands off to
        // this one cannot post before anyone is listening.
        DistributedNotificationCenter.default().addObserver(
            self, selector: #selector(showWindowRequested),
            name: LaunchPolicy.showWindowNotification, object: nil
        )
        if handOffToRunningCopy() { return }
        AppearanceChoice.stored().apply()
        model.showWindow = { [weak self] in self?.showMainWindow() }
        model.start()
        if LaunchPolicy.opensWindowAtLaunch(launchedAsLoginItem: launchedAsLoginItem) {
            showMainWindow()
        }
    }

    /// Opening the app again while it runs (Finder, Spotlight, `open`).
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showMainWindow()
        return false
    }

    /// Shows the main window, creating it the first time; it is kept when
    /// closed so it reopens where it was, on the pane it showed.
    func showMainWindow() {
        if window == nil {
            let root = MainWindow(
                monitor: model.monitor, prMonitor: model.prMonitor, restart: model.restart,
                search: model.search, sources: model.sources, tasks: model.tasks, dispatch: model.dispatch,
                github: model.github, prs: model.prs, handoff: model.handoff, landings: model.landings, system: model.system, logs: model.logs,
                docs: model.docs, registeredSources: model.registeredSources,
                safetyNet: model.safetyNet, claude: model.claude, config: model.config, docsOwners: model.docsOwners,
                enforcement: model.enforcement, placement: model.placement, spaces: model.spaces,
                spacesActivity: model.spacesActivity, spacesReview: model.spacesReview,
                loginItem: model.loginItem, navigator: model.navigator,
                window: model.windowControl
            ).environment(model.relabel).environment(model.directMessages)
            let hosting = NSHostingController(rootView: root)
            // The window keeps the size the user gives it. By default the
            // hosting controller resizes the window to each pane's ideal size,
            // so switching panes made it jump.
            hosting.sizingOptions = []
            // The window takes its first size from the view. A zero-sized view
            // would be clamped to the minimum, laying out the narrow companion
            // column before the saved frame applies, and rebuilding the panes
            // (cancelling what they were loading) once it does.
            hosting.view.setFrameSize(NSSize(width: 980, height: 640))
            let window = NSWindow(contentViewController: hosting)
            window.title = "Orchestrator"
            window.styleMask.insert([.resizable, .miniaturizable, .closable, .titled])
            // Narrow enough for the companion column the layout turns into.
            window.contentMinSize = CompanionLayout.minimumSize
            window.isReleasedWhenClosed = false
            window.setContentSize(NSSize(width: 980, height: 640))
            if !window.setFrameUsingName("OrchMainWindow") { window.center() }
            window.setFrameAutosaveName("OrchMainWindow")
            window.delegate = self
            model.windowControl.window = window
            self.window = window
        }
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    @objc private func showWindowRequested(_ notification: Notification) {
        let now = Date()
        guard LaunchPolicy.honours(request: now, lastHonoured: lastRequest) else { return }
        lastRequest = now
        showMainWindow()
    }

    /// When another copy is already running, asks it to show its window and
    /// quits this one, so there is never more than one tray icon.
    private func handOffToRunningCopy() -> Bool {
        guard let id = Bundle.main.bundleIdentifier else { return false }
        let current = NSRunningApplication.current
        let me = LaunchPolicy.Copy(pid: current.processIdentifier, launched: current.launchDate)
        let others = NSRunningApplication.runningApplications(withBundleIdentifier: id).map {
            LaunchPolicy.Copy(pid: $0.processIdentifier, launched: $0.launchDate, terminated: $0.isTerminated)
        }
        guard LaunchPolicy.shouldHandOff(me: me, others: others) else { return false }
        DistributedNotificationCenter.default().removeObserver(self)
        DistributedNotificationCenter.default().postNotificationName(
            LaunchPolicy.showWindowNotification, object: nil, userInfo: nil, deliverImmediately: true
        )
        NSApp.terminate(nil)
        return true
    }

    /// The GitHub and System tabs poll while open; a closed window (kept, not
    /// released) must not keep them polling.
    func windowWillClose(_ notification: Notification) {
        model.github.stop()
        model.system.stop()
        model.logs.stop()
    }
}
