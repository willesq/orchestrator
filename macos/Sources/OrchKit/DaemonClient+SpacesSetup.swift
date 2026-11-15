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

    public func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        var request = Orchd_V1_SetSpacesDraftRequest()
        request.policyJson = policyJSON
        let r = try await orchd.setSpacesDraft(request)
        return SavedSpacesDraft(revision: r.revision, sha256: r.sha256, problems: r.problems.map(Self.spacesProblem))
    }

    public func diffSpaces() async throws -> SpacesPolicyDiff {
        let r = try await orchd.diffSpaces(Orchd_V1_DiffSpacesRequest())
        return SpacesPolicyDiff(
            appliedRevision: r.appliedRevision, appliedJSON: r.appliedJson, draftRevision: r.draftRevision,
            changes: r.changes.map { SpacesPolicyChange(space: $0.space, kind: $0.kind, fields: $0.fields, details: $0.details) },
            problems: r.problems.map(Self.spacesProblem), draftCreatedBy: r.draftCreatedBy, draftByOperator: r.draftByOperator
        )
    }

    public func applySpaces(reviewedRevision: Int64) async throws -> Int64 {
        var request = Orchd_V1_ApplySpacesRequest()
        request.reviewedRevision = reviewedRevision
        // Reason: the wizard applies only the draft it saved itself, never an
        // agent's, so it never accepts an agent draft or a changed org.
        request.acceptAgentDraft = false
        return try await orchd.applySpaces(request).appliedRevision
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

    static func spacesProblem(_ p: Orchd_V1_SpacesProblem) -> SpacesProblem {
        SpacesProblem(rule: p.rule, space: p.space, message: p.message)
    }
}
