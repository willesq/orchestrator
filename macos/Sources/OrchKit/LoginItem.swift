import Foundation
import Observation
import ServiceManagement

/// The slice of `SMAppService` the login item uses, so tests need not touch
/// the real login items.
public protocol LoginItemService: Sendable {
    var status: SMAppService.Status { get }
    func register() throws
    func unregister() throws
}

extension SMAppService: LoginItemService {}

/// Whether Orch.app opens at login.
@MainActor
@Observable
public final class LoginItem {
    public enum State: Equatable, Sendable {
        case enabled
        case disabled
        /// Registered, but the user has to allow it in System Settings → Login Items.
        case needsApproval
        /// macOS cannot register this copy (e.g. a bare binary outside an app bundle).
        case unavailable
    }

    public private(set) var state: State = .disabled
    public private(set) var lastError: String?

    private let service: any LoginItemService

    public init(service: any LoginItemService = SMAppService.mainApp) {
        self.service = service
        refresh()
    }

    public var isEnabled: Bool { state == .enabled || state == .needsApproval }

    /// Re-reads the state; macOS can change it from System Settings.
    public func refresh() {
        state = Self.state(for: service.status)
    }

    public func setEnabled(_ enabled: Bool) {
        do {
            if enabled { try service.register() } else { try service.unregister() }
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
        refresh()
    }

    static func state(for status: SMAppService.Status) -> State {
        switch status {
        case .enabled: .enabled
        case .requiresApproval: .needsApproval
        case .notRegistered: .disabled
        case .notFound: .unavailable
        @unknown default: .unavailable
        }
    }
}
