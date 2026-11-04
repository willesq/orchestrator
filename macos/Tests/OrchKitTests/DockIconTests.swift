import AppKit
import Foundation
import Testing
@testable import OrchKit

@Suite struct DockIconTests {
    private func defaults(_ stored: Any? = nil) -> UserDefaults {
        let suite = "DockIconTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defaults.removePersistentDomain(forName: suite)
        if let stored { defaults.set(stored, forKey: DockIcon.key) }
        return defaults
    }

    @Test func shownUntilTheUserTurnsItOff() {
        #expect(DockIcon.shown(in: defaults()))
        #expect(DockIcon.shown(in: defaults(true)))
        #expect(!DockIcon.shown(in: defaults(false)))
    }

    @Test func anUnreadableValueCountsAsShown() {
        #expect(DockIcon.shown(in: defaults("sometimes")))
    }

    @Test func hidingKeepsTheAppRunningInTheMenuBar() {
        #expect(DockIcon.policy(shown: true) == .regular)
        #expect(DockIcon.policy(shown: false) == .accessory)
    }
}
