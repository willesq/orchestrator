import Foundation

/// When Orch.app shows its main window on its own.
public enum LaunchPolicy {
    /// Whether a launch should show the window: yes when the user opened the
    /// app, no when macOS started it at login, where only the tray icon belongs.
    public static func opensWindowAtLaunch(launchedAsLoginItem: Bool) -> Bool {
        !launchedAsLoginItem
    }

    /// The distributed notification a second copy posts so the running copy
    /// shows its window. It carries nothing, so anyone posting it can at most
    /// bring the window forward.
    public static let showWindowNotification = Notification.Name("com.willesq.orch.show-main-window")

    /// One running copy of the app, as NSRunningApplication reports it.
    public struct Copy: Sendable, Equatable {
        public let pid: Int32
        public let launched: Date?
        public let terminated: Bool

        public init(pid: Int32, launched: Date?, terminated: Bool = false) {
            self.pid = pid
            self.launched = launched
            self.terminated = terminated
        }
    }

    /// Whether `me` should hand off and quit. The earliest-launched live copy
    /// stays (lowest pid on a tie), so when copies start together exactly one
    /// survives instead of each quitting in favour of the other.
    public static func shouldHandOff(me: Copy, others: [Copy]) -> Bool {
        others.contains { other in
            guard !other.terminated, other.pid != me.pid else { return false }
            return precedes(other, me)
        }
    }

    private static func precedes(_ a: Copy, _ b: Copy) -> Bool {
        switch (a.launched, b.launched) {
        case let (x?, y?) where x != y: x < y
        case (_?, nil): true
        case (nil, _?): false
        default: a.pid < b.pid
        }
    }

    /// Spacing between honoured window requests, so a process posting the
    /// notification in a loop cannot keep pulling the window forward.
    public static let minimumRequestInterval: TimeInterval = 1

    public static func honours(request at: Date, lastHonoured: Date?) -> Bool {
        guard let lastHonoured else { return true }
        return at.timeIntervalSince(lastHonoured) >= minimumRequestInterval
    }
}
