import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
@testable import OrchKit

/// Serves a canned `Status` response on a Unix socket, so DaemonClient is
/// exercised over a real gRPC connection without a running orchd.
struct FakeStatusService: RegistrableRPCService {
    let response: Orchd_V1_StatusResponse

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let response = self.response
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Status.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_StatusRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_StatusResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: response))
        }
    }
}

/// Runs `body` with a fake orchd listening at a fresh socket path.
func withFakeOrchd(
    _ response: Orchd_V1_StatusResponse,
    _ body: (String) async throws -> Void
) async throws {
    let dir = try TempDir()
    let socket = dir.url.appendingPathComponent("orchd.sock").path
    let server = GRPCServer(
        transport: HTTP2ServerTransport.Posix(
            address: .unixDomainSocket(path: socket),
            transportSecurity: .plaintext
        ),
        services: [FakeStatusService(response: response)]
    )
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask { try await server.serve() }
        defer { server.beginGracefulShutdown() }
        let deadline = ContinuousClock.now + .seconds(5)
        while !FileManager.default.fileExists(atPath: socket) {
            guard ContinuousClock.now < deadline else {
                throw RPCError(code: .unavailable, message: "fake orchd never listened on \(socket)")
            }
            try await Task.sleep(for: .milliseconds(10))
        }
        try await body(socket)
    }
}
