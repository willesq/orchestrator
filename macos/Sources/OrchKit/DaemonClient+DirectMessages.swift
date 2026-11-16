import Foundation
import GRPCCore

/// The direct-message search (SP-083): orchd's SearchDirectMessages, which
/// orchd allows only for the operator, Orch.app under a policy (D-81).
extension DaemonClient: DirectMessagesAPI {
    public func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult {
        let search = Self.searchRequest(query.normalized)
        var request = Orchd_V1_SearchDirectMessagesRequest()
        request.search = search
        var options = CallOptions.defaults
        options.timeout = searchTimeout
        let response = try await orchd.searchDirectMessages(request, options: options)
        return DirectMessageResult(
            hits: response.hits.enumerated().map { i, h in Self.directMessageHit(h, index: i) },
            fallback: response.fallback.isEmpty ? nil : displaySafe(response.fallback)
        )
    }

    /// A hit as the view shows it. Chat text is written by other people, so
    /// every field is made safe to display before it leaves OrchKit.
    static func directMessageHit(_ h: Orchd_V1_SearchHit, index: Int) -> DirectMessageHit {
        DirectMessageHit(
            index: index, text: displaySafeLines(h.text), sourceURI: displaySafe(h.sourceUri),
            tenant: displaySafe(h.tenant), space: displaySafe(h.space), score: h.score,
            startedAt: date(fromUnix: h.startedAt)
        )
    }
}
