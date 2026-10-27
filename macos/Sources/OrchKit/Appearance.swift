import AppKit

/// The app's light or dark choice: follow the system (the default), or keep
/// one regardless of it. Stored in user defaults under `key`.
public enum AppearanceChoice: String, CaseIterable, Identifiable, Sendable {
    case system, light, dark

    /// The user defaults key the choice is stored under.
    public static let key = "appearance"

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .system: "System"
        case .light: "Light"
        case .dark: "Dark"
        }
    }

    /// The appearance to force, or nil to follow the system.
    public var appearanceName: NSAppearance.Name? {
        switch self {
        case .system: nil
        case .light: .aqua
        case .dark: .darkAqua
        }
    }

    /// The stored choice; anything unknown or missing follows the system.
    public static func stored(in defaults: UserDefaults = .standard) -> AppearanceChoice {
        defaults.string(forKey: key).flatMap(AppearanceChoice.init(rawValue:)) ?? .system
    }

    /// Applies the choice to every window and the menu bar panel at once.
    @MainActor
    public func apply(to app: NSApplication = .shared) {
        app.appearance = appearanceName.flatMap(NSAppearance.init(named:))
    }
}
