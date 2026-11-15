import Foundation

/// The operator unlocks (SP-077), for the menu bar panel.
extension DaemonClient: UnlocksAPI {
    public func unlocks() async throws -> [OperatorUnlock] {
        try await orchd.listUnlocks(Orchd_V1_ListUnlocksRequest()).unlocks.compactMap(Self.unlock)
    }

    public func confirmUnlock(id: String, allow: Bool) async throws {
        var request = Orchd_V1_ConfirmUnlockRequest()
        request.id = id
        request.allow = allow
        _ = try await orchd.confirmUnlock(request)
    }

    public func lockUnlock(id: String) async throws -> Int {
        var request = Orchd_V1_LockRequest()
        request.id = id
        return Int(try await orchd.lock(request).ended)
    }

    /// An unlock as the app shows it; one in a state this app does not know
    /// is left out rather than offered for confirmation.
    static func unlock(_ u: Orchd_V1_OperatorUnlock) -> OperatorUnlock? {
        guard let state = OperatorUnlock.State(rawValue: u.state) else { return nil }
        return OperatorUnlock(
            id: u.id, state: state, forSeconds: u.forSeconds, pid: u.pid,
            command: Self.unlockSafe(u.command), ancestry: u.ancestry.map(Self.unlockSafe),
            requestedAt: Date(timeIntervalSince1970: TimeInterval(u.requestedAt)),
            expiresAt: Date(timeIntervalSince1970: TimeInterval(u.expiresAt)),
            code: Self.unlockSafe(u.code), executable: Self.unlockSafe(u.executable), tty: Self.unlockSafe(u.tty)
        )
    }

    /// displaySafe, with line and paragraph separators too, which it keeps
    /// and which could push text out of a line-limited view.
    static func unlockSafe(_ s: String) -> String {
        displaySafe(s).replacingOccurrences(of: "\u{2028}", with: "?").replacingOccurrences(of: "\u{2029}", with: "?")
    }
}
