import Foundation
import Observation

/// One login a space uses, as orchd read it from `gh auth status` or
/// `claude auth status`: status metadata only, never a token (SP-074).
public struct LoginState: Sendable, Equatable {
    /// The space names its own folder; false is the default login.
    public var own: Bool
    public var dir: String
    /// logged in | not logged in | refused | unknown
    public var state: String
    /// The gh account, or Claude Code's sign-in method.
    public var account: String
    /// Where gh keeps the token: keyring, file, or a variable.
    public var storage: String
    public var detail: String

    public init(own: Bool = false, dir: String = "", state: String = "", account: String = "", storage: String = "", detail: String = "") {
        self.own = own
        self.dir = dir
        self.state = state
        self.account = account
        self.storage = storage
        self.detail = detail
    }

    public var isLoggedIn: Bool { state == "logged in" }

    /// "Default login, logged in as willesq (token in keyring)".
    public func summary(defaultName: String, ownName: String) -> String {
        var text = (own ? ownName : defaultName) + ", " + displaySafe(state)
        if !account.isEmpty { text += " as " + displaySafe(account) }
        if !storage.isEmpty { text += " (token in \(displaySafe(storage)))" }
        return text
    }
}

/// A space's gh login and Claude Code configuration.
public struct SpaceLogin: Sendable, Equatable, Identifiable {
    public var space: String
    public var gh: LoginState
    public var claude: LoginState
    public var id: String { space }

    public init(space: String, gh: LoginState, claude: LoginState) {
        self.space = space
        self.gh = gh
        self.claude = claude
    }

    /// Something the user should fix: a login that is not working.
    public var needsAttention: Bool {
        [gh, claude].contains { $0.state == "not logged in" || $0.state == "refused" }
    }
}

/// Reads the spaces' logins for Settings → Spaces.
@MainActor
@Observable
public final class SpaceLoginsModel {
    public private(set) var logins: [SpaceLogin]?
    public private(set) var lastError: String?
    private let api: any SpacesAPI
    private var generation = 0

    public init(api: any SpacesAPI) {
        self.api = api
    }

    public func load() async {
        generation += 1
        let mine = generation
        do {
            let loaded = try await api.spaceLogins()
            guard mine == generation else { return }
            logins = loaded
            lastError = nil
        } catch {
            guard mine == generation else { return }
            lastError = spacesRefusal(error)
        }
    }
}
