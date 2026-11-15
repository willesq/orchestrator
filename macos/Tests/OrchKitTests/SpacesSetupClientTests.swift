import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

/// The setup wizard's RPCs, over a real gRPC connection to a fake orchd.
@Suite struct SpacesSetupClientTests {
    @Test func daemonClientScansDraftsDiffsAndApplies() async throws {
        var scan = Orchd_V1_ScanSpacesResponse()
        var c = Orchd_V1_ScannedCheckout()
        c.path = "/w/acme/api"
        c.owner = "acme"
        c.repo = "api"
        c.visibility = "private"
        c.anonymous = true
        c.space = "acme"
        c.rule = "root"
        c.outcome = "placed"
        c.reason = "r"
        scan.checkouts = [c]
        var a = Orchd_V1_ScannedAccount()
        a.profile = "acme"
        a.login = "me"
        a.orgs = ["acme"]
        a.error = "e"
        scan.accounts = [a]
        scan.truncated = true
        scan.problems = ["p"]
        var draft = Orchd_V1_SpacesDraft()
        draft.revision = 4
        draft.sha256 = "abc"
        var problem = Orchd_V1_SpacesProblem()
        problem.rule = "bad_binding"
        problem.space = "acme"
        problem.message = "m"
        draft.problems = [problem]
        var diff = Orchd_V1_SpacesDiff()
        diff.appliedRevision = 3
        diff.appliedJson = "{}"
        diff.draftRevision = 4
        var change = Orchd_V1_SpacesChange()
        change.space = "acme"
        change.kind = "added"
        change.fields = ["roots"]
        change.details = ["d"]
        diff.changes = [change]
        diff.problems = [problem]
        diff.draftCreatedBy = "operator"
        diff.draftByOperator = true
        var applied = Orchd_V1_ApplySpacesResponse()
        applied.appliedRevision = 4
        let service = FakeSetupService(scan: scan, draft: draft, diff: diff, applied: applied)

        var config = FakeOrchdService()
        var field = Orchd_V1_ConfigField()
        field.key = CommandLogRetention.key
        field.value = "\"30d\""
        field.options = ["3d", "7d", "30d"]
        field.live = false
        config.configState.fields = [field]

        try await withFakeOrchd(services: [service, config]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }

            let s = try await client.scanSpaces(folders: ["/w"], offline: true)
            let sent = service.recorder.last(Orchd_V1_ScanSpacesRequest.self)
            #expect(sent?.folders == ["/w"])
            #expect(sent?.offline == true)
            #expect(s == SpacesScan(
                checkouts: [ScannedCheckout(path: "/w/acme/api", owner: "acme", repo: "api", visibility: "private", anonymous: true,
                                            space: "acme", rule: "root", outcome: "placed", reason: "r")],
                accounts: [ScannedAccount(profile: "acme", login: "me", orgs: ["acme"], error: "e")], truncated: true, problems: ["p"]))
            #expect(s.checkouts.first?.hasRemote == false)

            let d = try await client.saveSpacesDraft(policyJSON: "{\"spaces\":[]}")
            #expect(service.recorder.last(Orchd_V1_SetSpacesDraftRequest.self)?.policyJson == "{\"spaces\":[]}")
            #expect(d == SavedSpacesDraft(revision: 4, sha256: "abc", problems: [SpacesProblemInfo(rule: "bad_binding", space: "acme", message: "m")]))

            let df = try await client.diffSpaces()
            #expect(df == SpacesPolicyDiff(appliedRevision: 3, appliedJSON: "{}", draftRevision: 4,
                                           changes: [SpacesPolicyChange(space: "acme", kind: "added", fields: ["roots"], details: ["d"])],
                                           problems: [SpacesProblemInfo(rule: "bad_binding", space: "acme", message: "m")],
                                           draftCreatedBy: "operator", draftByOperator: true))

            #expect(try await client.applySpaces(reviewedRevision: 4, acceptAgentDraft: false, confirmGitHubOrgs: []) == 4)
            let apply = service.recorder.last(Orchd_V1_ApplySpacesRequest.self)
            #expect(apply?.reviewedRevision == 4)
            #expect(apply?.acceptAgentDraft == false)
            #expect(apply?.confirmGithubOrgs == [])

            #expect(try await client.commandLogRetention() == CommandLogRetention(value: "30d", needsRestart: true))
            try await client.setCommandLogRetention("3d")
            let set = config.recorder.last(Orchd_V1_SetConfigRequest.self)
            #expect(set?.values == [CommandLogRetention.key: "\"3d\""])
            #expect(set?.confirmSensitive == true)
        }

        // An orchd without the setting falls back to the default.
        try await withFakeOrchd(services: [FakeOrchdService()]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            #expect(try await client.commandLogRetention() == CommandLogRetention(value: "7d"))
        }
    }
}

/// ScanSpaces, SetSpacesDraft, DiffSpaces and ApplySpaces, served like orchd.
private struct FakeSetupService: RegistrableRPCService {
    let scan: Orchd_V1_ScanSpacesResponse
    let draft: Orchd_V1_SpacesDraft
    let diff: Orchd_V1_SpacesDiff
    let applied: Orchd_V1_ApplySpacesResponse
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let scan = scan, draft = draft, diff = diff, applied = applied, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ScanSpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ScanSpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ScanSpacesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: scan))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetSpacesDraft.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetSpacesDraftRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpacesDraft>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: draft))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.DiffSpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_DiffSpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpacesDiff>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: diff))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ApplySpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ApplySpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ApplySpacesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: applied))
        }
    }
}
