import Foundation
import ServiceManagement
import Testing
@testable import OrchKit

private struct RegisterError: Error, LocalizedError {
    var errorDescription: String? { "Operation not permitted" }
}

private final class FakeService: LoginItemService, @unchecked Sendable {
    var status: SMAppService.Status
    var failNext = false
    private(set) var calls: [String] = []

    init(_ status: SMAppService.Status) { self.status = status }

    func register() throws {
        calls.append("register")
        if failNext { failNext = false; throw RegisterError() }
        status = .enabled
    }

    func unregister() throws {
        calls.append("unregister")
        if failNext { failNext = false; throw RegisterError() }
        status = .notRegistered
    }
}

@MainActor
@Suite struct LoginItemTests {
    @Test(arguments: [
        (SMAppService.Status.enabled, LoginItem.State.enabled, true),
        (.notRegistered, .disabled, false),
        (.requiresApproval, .needsApproval, true),
        (.notFound, .unavailable, false),
    ])
    func readsInitialState(status: SMAppService.Status, state: LoginItem.State, enabled: Bool) {
        let item = LoginItem(service: FakeService(status))
        #expect(item.state == state)
        #expect(item.isEnabled == enabled)
    }

    @Test func enableThenDisable() {
        let service = FakeService(.notRegistered)
        let item = LoginItem(service: service)
        item.setEnabled(true)
        #expect(item.state == .enabled && item.lastError == nil)
        item.setEnabled(false)
        #expect(item.state == .disabled)
        #expect(service.calls == ["register", "unregister"])
    }

    @Test func failureKeepsRealStateAndReportsIt() {
        let service = FakeService(.notRegistered)
        service.failNext = true
        let item = LoginItem(service: service)
        item.setEnabled(true)
        #expect(item.state == .disabled)
        #expect(item.lastError == "Operation not permitted")
        item.setEnabled(true)
        #expect(item.state == .enabled && item.lastError == nil, "a later success clears the error")
    }

    @Test func refreshPicksUpChangesMadeInSystemSettings() {
        let service = FakeService(.enabled)
        let item = LoginItem(service: service)
        service.status = .requiresApproval
        item.refresh()
        #expect(item.state == .needsApproval)
    }
}
