import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2

/// The orchd calls the app depends on, behind a protocol so views and the
/// monitor can be tested without a daemon.
public protocol DaemonAPI: Sendable {
    func status() async throws -> DaemonStatus
}

/// Talks to orchd over its Unix socket. One long-lived HTTP/2 connection is
/// shared by every call; the transport reconnects on its own when orchd
/// restarts and recreates the socket.
public final class DaemonClient: DaemonAPI, Sendable {
    private let client: GRPCClient<HTTP2ClientTransport.Posix>
    private let orchd: Orchd_V1_Orchd.Client<HTTP2ClientTransport.Posix>
    private let callTimeout: Duration

    public init(socketPath: String = OrchPaths.socketPath(), callTimeout: Duration = .seconds(3)) throws {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: socketPath),
            transportSecurity: .plaintext
        )
        client = GRPCClient(transport: transport)
        orchd = Orchd_V1_Orchd.Client(wrapping: client)
        self.callTimeout = callTimeout
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
        var options = CallOptions.defaults
        options.timeout = callTimeout
        let response = try await orchd.status(Orchd_V1_StatusRequest(), options: options)
        return DaemonStatus(
            version: response.version,
            uptimeSeconds: response.uptimeSeconds,
            subsystems: response.subsystems.map {
                Subsystem(name: $0.name, state: $0.state, detail: $0.detail, error: $0.error, restarts: Int($0.restarts))
            }
        )
    }
}
