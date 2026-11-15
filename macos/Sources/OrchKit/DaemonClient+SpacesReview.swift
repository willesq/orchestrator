import Foundation

/// The Spaces pane's Review and Activity pages: the pending draft, saving
/// and applying it, and one session's command log.
extension DaemonClient: SpacesReviewAPI, SpacesActivityAPI {
    public func diffSpaces() async throws -> SpacesPolicyDiff {
        let r = try await orchd.diffSpaces(Orchd_V1_DiffSpacesRequest())
        return SpacesPolicyDiff(
            appliedRevision: r.appliedRevision, appliedJSON: r.appliedJson, draftRevision: r.draftRevision,
            draftJSON: r.draftJson,
            changes: r.changes.map { SpacesPolicyChange(space: $0.space, kind: $0.kind, fields: $0.fields, details: $0.details) },
            problems: r.problems.map(Self.problem), draftCreatedBy: r.draftCreatedBy,
            draftCreatedAt: r.draftCreatedAt > 0 ? Date(timeIntervalSince1970: TimeInterval(r.draftCreatedAt)) : nil,
            draftByOperator: r.draftByOperator, unconfirmedGitHubOrgs: r.unconfirmedGithubOrgs
        )
    }

    public func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        var request = Orchd_V1_SetSpacesDraftRequest()
        request.policyJson = policyJSON
        let r = try await orchd.setSpacesDraft(request)
        return SavedSpacesDraft(revision: r.revision, sha256: r.sha256, problems: r.problems.map(Self.problem))
    }

    public func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64 {
        var request = Orchd_V1_ApplySpacesRequest()
        request.reviewedRevision = reviewedRevision
        request.acceptAgentDraft = acceptAgentDraft
        request.confirmGithubOrgs = confirmGitHubOrgs
        return try await orchd.applySpaces(request).appliedRevision
    }

    public func toolEvents(owner: String, afterSeq: Int64, limit: Int32) async throws -> ToolLogPage {
        var request = Orchd_V1_ListToolEventsRequest()
        request.owner = owner
        request.afterSeq = afterSeq
        request.limit = limit
        let r = try await orchd.listToolEvents(request)
        return ToolLogPage(
            events: r.events.map {
                ToolLogEvent(seq: $0.seq, at: Date(timeIntervalSince1970: TimeInterval($0.at) / 1_000_000_000),
                             kind: $0.kind, event: $0.event, tool: $0.tool, agentType: $0.agentType, cwd: $0.cwd,
                             oldCwd: $0.oldCwd, newCwd: $0.newCwd, command: $0.command, targets: $0.targets,
                             decision: $0.decision, rule: $0.rule, dropped: $0.dropped)
            },
            more: r.more
        )
    }

    static func problem(_ p: Orchd_V1_SpacesProblem) -> SpacesProblemInfo {
        SpacesProblemInfo(rule: p.rule, space: p.space, message: p.message)
    }
}
