import Foundation

/// The Spaces pane's advanced editors (SP-053). The draft, sessions,
/// switches, placement and source calls are the ones other features use;
/// these add terms, quarantine, retag and the per-space counts.
extension DaemonClient: SpacesEditorAPI {
    public func spaceTerms() async throws -> [SpaceTermsInfo] {
        try await orchd.getSpaceTerms(Orchd_V1_GetSpaceTermsRequest()).spaces.map(Self.terms)
    }

    public func setSpaceTerms(space: String, terms: String) async throws -> SpaceTermsInfo {
        var request = Orchd_V1_SetSpaceTermsRequest()
        request.space = space
        request.terms = terms
        return Self.terms(try await orchd.setSpaceTerms(request))
    }

    public func quarantinedSources() async throws -> [QuarantinedSource] {
        try await orchd.listQuarantine(Orchd_V1_ListQuarantineRequest()).sources.map {
            QuarantinedSource(uri: $0.sourceUri, sourceType: $0.sourceType, chunks: Int($0.chunks))
        }
    }

    public func retagSource(uri: String, space: String, confirm: Bool, expectFrom: String) async throws -> RetagOutcome {
        var request = Orchd_V1_RetagSourceRequest()
        request.sourceUri = uri
        request.space = space
        request.confirm = confirm
        request.expectFrom = expectFrom
        let r = try await orchd.retagSource(request)
        return RetagOutcome(from: r.from, chunks: r.chunks, moved: r.moved)
    }

    public func spaceChunkCounts() async throws -> [SpaceChunkCount] {
        let counts = try await orchd.status(Orchd_V1_StatusRequest()).spaces.counts
        return counts.spaces.map { SpaceChunkCount(space: $0.space, chunks: Int($0.chunks), stale: Int($0.stale)) }
    }

    private static func terms(_ t: Orchd_V1_SpaceTerms) -> SpaceTermsInfo {
        SpaceTermsInfo(space: t.space, termsFile: t.termsFile, exists: t.exists, count: Int(t.count),
                       fingerprint: t.fingerprint, problem: t.problem)
    }
}
