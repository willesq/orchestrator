import Foundation

/// The Spaces pane's Overview: the applied policy, sessions, the command
/// log summary and unconfirmed GitHub orgs.
extension DaemonClient: SpacesOverviewAPI {
    public func listSpaces() async throws -> SpacesListing {
        let r = try await orchd.listSpaces(Orchd_V1_ListSpacesRequest())
        let s = r.status
        return SpacesListing(
            status: SpacesPolicyStatus(configured: s.configured, appliedRevision: s.appliedRevision,
                                       draftRevision: s.draftRevision, drift: s.drift, error: s.error),
            spaces: r.spaces.map {
                SpaceInfo(name: $0.name, kind: $0.kind, githubOrgs: $0.githubOrgs, roots: $0.roots,
                          webPrefixes: $0.webPrefixes, chat: $0.chat, reads: $0.reads, readable: $0.readable,
                          termsFile: $0.termsFile, ghConfigDir: $0.ghConfigDir)
            }
        )
    }

    public func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        var request = Orchd_V1_ListSessionsRequest()
        request.all = includeEnded
        return try await orchd.listSessions(request).sessions.map {
            PinnedSession(harness: $0.harness, sessionID: $0.sessionID, harnessPID: $0.harnessPid, space: $0.space,
                          firstCwd: $0.firstCwd, state: $0.state,
                          lastActivity: $0.lastActivity > 0 ? Date(timeIntervalSince1970: TimeInterval($0.lastActivity)) : nil,
                          toolEvents: $0.toolEvents, refusals: $0.refusals)
        }
    }

    public func toolLogSummary() async throws -> [SpaceActivity] {
        try await orchd.summarizeToolLog(Orchd_V1_SummarizeToolLogRequest()).spaces.map {
            SpaceActivity(space: $0.space, sessions: Int($0.sessions), toolCalls: $0.toolCalls,
                          directoryMoves: $0.directoryMoves, crossSpaceMoves: $0.crossSpaceMoves, refusals: $0.refusals)
        }
    }

    public func unconfirmedGitHubOrgs() async throws -> [String] {
        try await orchd.diffSpaces(Orchd_V1_DiffSpacesRequest()).unconfirmedGithubOrgs
    }
}
