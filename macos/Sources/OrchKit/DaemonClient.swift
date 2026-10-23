import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2

/// The orchd calls the app depends on, behind a protocol so views and the
/// monitor can be tested without a daemon.
public protocol DaemonAPI: Sendable {
    func status() async throws -> DaemonStatus
    /// Every cached PR, most urgent first.
    func listPRs() async throws -> PRList
}

/// Talks to orchd over its Unix socket. One long-lived HTTP/2 connection is
/// shared by every call; the transport reconnects on its own when orchd
/// restarts and recreates the socket.
public final class DaemonClient: DaemonAPI, MemoryAPI, ControlAPI, Sendable {
    private let client: GRPCClient<HTTP2ClientTransport.Posix>
    private let orchd: Orchd_V1_Orchd.Client<HTTP2ClientTransport.Posix>
    private let callTimeout: Duration
    private let searchTimeout: Duration

    /// `searchTimeout` is longer because the first search after a restart
    /// loads the embedding model.
    public init(
        socketPath: String = OrchPaths.socketPath(), callTimeout: Duration = .seconds(3),
        searchTimeout: Duration = .seconds(15)
    ) throws {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: socketPath),
            transportSecurity: .plaintext
        )
        client = GRPCClient(transport: transport)
        orchd = Orchd_V1_Orchd.Client(wrapping: client)
        self.callTimeout = callTimeout
        self.searchTimeout = searchTimeout
    }

    /// Runs the connection until `close()` or task cancellation. Call once,
    /// from a long-lived task, before issuing RPCs.
    public func run() async throws {
        try await client.runConnections()
    }

    public func close() {
        client.beginGracefulShutdown()
    }

    public func status() async throws -> DaemonStatus {
        let response = try await orchd.status(Orchd_V1_StatusRequest(), options: callOptions)
        return DaemonStatus(
            version: response.version,
            uptimeSeconds: response.uptimeSeconds,
            subsystems: response.subsystems.map {
                Subsystem(name: $0.name, state: $0.state, detail: $0.detail, error: $0.error, restarts: Int($0.restarts))
            }
        )
    }

    public func listPRs() async throws -> PRList {
        let response = try await orchd.listPRs(Orchd_V1_ListPRsRequest(), options: callOptions)
        return PRList(
            prs: response.prs.map {
                PullRequest(
                    url: $0.url, title: $0.title, repo: $0.repo, ciState: $0.ciState,
                    reviewDecision: $0.reviewDecision, mergeable: $0.mergeable, isDraft: $0.isDraft,
                    reasons: $0.reasons, attention: Attention(rawValue: $0.attention)
                )
            },
            updatedAt: response.updatedAt > 0 ? Date(timeIntervalSince1970: TimeInterval(response.updatedAt)) : nil
        )
    }

    public func search(_ query: SearchQuery) async throws -> SearchResult {
        let q = query.normalized
        var request = Orchd_V1_SearchRequest()
        request.query = q.text
        request.topK = Int32(q.limit)
        request.sourceType = q.sourceType
        request.scope = q.scope
        request.mode = q.mode.rawValue
        var options = CallOptions.defaults
        options.timeout = searchTimeout
        let response = try await orchd.search(request, options: options)
        return SearchResult(
            hits: response.hits.map {
                SearchHit(
                    text: $0.text, sourceURI: $0.sourceUri, sourceType: $0.sourceType, scope: $0.scope,
                    score: $0.score, keywordScore: $0.keywordScore, exact: $0.exact, tags: $0.tags,
                    startedAt: date(fromUnix: $0.startedAt), ingestedAt: date(fromUnix: $0.ingestedAt)
                )
            },
            fallback: response.fallback.isEmpty ? nil : response.fallback
        )
    }

    public func listSources(type: String, limit: Int) async throws -> SourceList {
        var request = Orchd_V1_ListSourcesRequest()
        request.sourceType = type
        request.limit = Int32(clamping: max(limit, 0))
        let response = try await orchd.listSources(request, options: callOptions)
        return SourceList(
            sources: response.sources.map {
                SourceSummary(
                    uri: $0.sourceUri, sourceType: $0.sourceType, scope: $0.scope, chunks: Int($0.chunks),
                    tags: $0.tags, ingestedAt: date(fromUnix: $0.ingestedAt)
                )
            },
            totalSources: Int(response.totalSources),
            totalChunks: Int(response.totalChunks)
        )
    }

    public func restart(_ name: String?) async throws -> [String] {
        var request = Orchd_V1_RestartRequest()
        request.name = name ?? ""
        // Stopping a unit can take up to 15 s before the supervisor gives up.
        var options = CallOptions.defaults
        options.timeout = .seconds(40)
        return try await orchd.restart(request, options: options).restarted
    }

    private var callOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = callTimeout
        return options
    }
}
