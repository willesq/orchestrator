import Foundation
import GRPCCore
import GRPCProtobuf
@testable import OrchKit

/// The terms the editor tests send. None may ever show in a model, a
/// notice or a snapshot (SP-053.1.a2, INV-07).
let fixtureTerms = ["ProjectNightjar", "re:acme-[0-9]+", "hunter-two"]
let fixtureTermsText = "# job words\n" + fixtureTerms.joined(separator: "\n") + "\n"

/// A policy with every kind: oss public with a root, personal private
/// inside it, notes private, acme isolated inside, globex isolated.
let editorFixturePolicy = """
{"spaces":[
 {"name":"oss","kind":"public","roots":["~/code"],"public_paths":["~/code/tools/**"],"web_prefixes":["https://docs.oss.dev/"],"github_orgs":["oss-org"]},
 {"name":"personal","kind":"private","roots":["~/code/willesq"],"github_orgs":["willesq"],"reads":["notes"]},
 {"name":"notes","kind":"private"},
 {"name":"acme","kind":"isolated","roots":["~/code/company1"],"github_orgs":["acme"],"chat":["slack:TACME"],"terms_file":"~/.config/orch/terms/acme.txt","x_future":true},
 {"name":"globex","kind":"isolated","github_orgs":["globex"]}
],"public_root_sessions":"personal","x_top":[1,2]}
"""

/// Answers the editor's calls like orchd, recording the writes.
final class FakeEditorAPI: SpacesEditorAPI, @unchecked Sendable {
    private let lock = NSLock()
    var state = "on"
    var diff = SpacesPolicyDiff(appliedRevision: 4, appliedJSON: editorFixturePolicy)
    var diffError: (any Error)?
    var terms: [SpaceTermsInfo] = [
        SpaceTermsInfo(space: "acme", termsFile: "~/.config/orch/terms/acme.txt", exists: true, count: 2, fingerprint: "0a1b2c3d4e5f"),
        SpaceTermsInfo(space: "oss"),
    ]
    var termsRefusal: (any Error)?
    var sessions: [PinnedSession] = []
    var sources = SourceList(sources: [], totalSources: 0, totalChunks: 0)
    var quarantine: [QuarantinedSource] = []
    var quarantineError: (any Error)?
    var retagResult: Result<RetagOutcome, any Error> = .success(RetagOutcome(from: ["quarantine"], chunks: 3, moved: true))
    var saveResult: Result<SavedSpacesDraft, any Error> = .success(SavedSpacesDraft(revision: 5))
    var counts: [SpaceChunkCount] = [SpaceChunkCount(space: "acme", chunks: 120, stale: 4)]
    var placements: [String: PlacementExplanation] = [:]

    private(set) var saved: [String] = []
    /// Every terms text sent; the test checks it reached orchd and nothing else.
    private(set) var sentTerms: [(space: String, text: String)] = []
    private(set) var retags: [(uri: String, space: String, confirm: Bool, expectFrom: String)] = []

    func status() async throws -> DaemonStatus {
        DaemonStatus(version: "t", uptimeSeconds: 1, subsystems: [], spacesState: state)
    }
    func diffSpaces() async throws -> SpacesPolicyDiff {
        if let diffError { throw diffError }
        return lock.withLock { diff }
    }
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        lock.withLock { saved.append(policyJSON) }
        return try saveResult.get()
    }
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] { sessions }
    func enforcement() async throws -> EnforcementState { EnforcementState(switches: []) }
    func explainPlacement(target: String) async throws -> PlacementExplanation {
        guard let p = placements[target] else { throw RPCError(code: .invalidArgument, message: "no fixture for \(target)") }
        return p
    }
    func listSources(type: String, limit: Int) async throws -> SourceList { sources }
    func spaceTerms() async throws -> [SpaceTermsInfo] {
        if let termsRefusal { throw termsRefusal }
        return terms
    }
    func setSpaceTerms(space: String, terms text: String) async throws -> SpaceTermsInfo {
        lock.withLock { sentTerms.append((space, text)) }
        if let termsRefusal { throw termsRefusal }
        let n = text.split(separator: "\n").filter { !$0.hasPrefix("#") && !$0.isEmpty }.count
        return SpaceTermsInfo(space: space, termsFile: "~/.config/orch/terms/\(space).txt", exists: true, count: n, fingerprint: "ffeeddccbbaa")
    }
    func quarantinedSources() async throws -> [QuarantinedSource] {
        if let quarantineError { throw quarantineError }
        return quarantine
    }
    func retagSource(uri: String, space: String, confirm: Bool, expectFrom: String) async throws -> RetagOutcome {
        lock.withLock { retags.append((uri, space, confirm, expectFrom)) }
        return try retagResult.get()
    }
    func spaceChunkCounts() async throws -> [SpaceChunkCount] { counts }
}

/// GetSpaceTerms, SetSpaceTerms, ListQuarantine and RetagSource served like
/// orchd; Status comes from FakeOrchdService.
struct FakeEditorService: RegistrableRPCService {
    var terms = Orchd_V1_SpaceTermsList()
    var setTerms = Orchd_V1_SpaceTerms()
    var quarantine = Orchd_V1_ListQuarantineResponse()
    var retag = Orchd_V1_RetagSourceResponse()
    var refusal: RPCError?
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let terms = terms, setTerms = setTerms, quarantine = quarantine, retag = retag, refusal = refusal, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.GetSpaceTerms.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_GetSpaceTermsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpaceTermsList>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: terms))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetSpaceTerms.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetSpaceTermsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpaceTerms>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            if let refusal { return StreamingServerResponse(single: ServerResponse(error: refusal)) }
            return StreamingServerResponse(single: ServerResponse(message: setTerms))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListQuarantine.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListQuarantineRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListQuarantineResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: quarantine))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.RetagSource.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_RetagSourceRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_RetagSourceResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: retag))
        }
    }
}
