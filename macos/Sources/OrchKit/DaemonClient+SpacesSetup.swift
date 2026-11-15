import Foundation

/// The setup wizard's reads and writes (SP-052).
extension DaemonClient: SpacesSetupAPI {
    public func scanSpaces(folders: [String], offline: Bool) async throws -> SpacesScan {
        var request = Orchd_V1_ScanSpacesRequest()
        request.folders = folders
        request.offline = offline
        let r = try await orchd.scanSpaces(request)
        return SpacesScan(
            checkouts: r.checkouts.map {
                ScannedCheckout(path: $0.path, owner: $0.owner, repo: $0.repo, visibility: $0.visibility, anonymous: $0.anonymous,
                                space: $0.space, rule: $0.rule, outcome: $0.outcome, reason: $0.reason)
            },
            accounts: r.accounts.map { ScannedAccount(profile: $0.profile, login: $0.login, orgs: $0.orgs, error: $0.error) },
            truncated: r.truncated, problems: r.problems
        )
    }

    public func commandLogRetention() async throws -> CommandLogRetention {
        let state = try await config()
        guard let field = state.fields.first(where: { $0.key == CommandLogRetention.key }) else {
            return CommandLogRetention(value: "7d")
        }
        return CommandLogRetention(value: field.text, options: field.options.isEmpty ? ["3d", "7d", "30d"] : field.options,
                                   needsRestart: !field.live)
    }

    public func setCommandLogRetention(_ value: String) async throws {
        // A sensitive setting: the operator confirmed it by applying setup.
        _ = try await setConfig([CommandLogRetention.key: ConfigJSON.encode(value)], confirmSensitive: true)
    }
}
