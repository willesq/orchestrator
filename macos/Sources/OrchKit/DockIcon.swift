import AppKit

/// Whether the app shows a Dock icon alongside its menu bar icon: yes unless
/// the user turned it off. Stored in user defaults under `key`.
public enum DockIcon {
    /// The user defaults key the choice is stored under.
    public static let key = "showInDock"

    /// The stored choice; missing means shown.
    public static func shown(in defaults: UserDefaults = .standard) -> Bool {
        defaults.object(forKey: key) as? Bool ?? true
    }

    /// `.accessory` keeps the menu bar icon and the windows, and drops only
    /// the Dock icon, the app menu and the ⌘-Tab entry.
    public static func policy(shown: Bool) -> NSApplication.ActivationPolicy {
        shown ? .regular : .accessory
    }
}
