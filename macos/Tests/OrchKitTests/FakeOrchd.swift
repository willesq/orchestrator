import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
@testable import OrchKit

/// Serves canned `Status` and `ListPRs` responses on a Unix socket, so
/// DaemonClient is exercised over a real gRPC connection without a running
/// orchd.
struct FakeOrchdService: RegistrableRPCService {
    var status = Orchd_V1_StatusResponse()
    var prs = Orchd_V1_ListPRsResponse()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let status = self.status, prs = self.prs
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.Status.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_StatusRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_StatusResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: status))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListPRs.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListPRsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListPRsResponse>()
        ) { _, _ in
            StreamingServerResponse(single: ServerResponse(message: prs))
        }
    }
}

/// Runs `body` with a fake orchd listening at a fresh socket path.
func withFakeOrchd(
    _ service: FakeOrchdService,
    _ body: (String) async throws -> Void
) async throws {
    let dir = try TempDir()
    let socket = dir.url.appendingPathComponent("orchd.sock").path
    let server = GRPCServer(
        transport: HTTP2ServerTransport.Posix(
            address: .unixDomainSocket(path: socket),
            transportSecurity: .plaintext
        ),
        services: [service]
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
