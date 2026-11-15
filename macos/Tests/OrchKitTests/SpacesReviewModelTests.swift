import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

private let home = "/Users/me"
private let applied = #"{"spaces":[{"name":"acme","kind":"isolated","reads":["public"]},{"name":"globex","kind":"isolated"}]}"#
private let draft = #"{"spaces":[{"name":"acme","kind":"isolated","reads":["public","globex"]},{"name":"globex","kind":"isolated"}]}"#
private let readsGlobex = SpacesProblemInfo(rule: "isolated_reads_isolated", space: "acme",
                                            message: #"isolated space "acme" cannot read isolated space "globex"; merge them instead"#)

/// orchd's draft store in miniature: drafts get the next revision, the
/// newest is what DiffSpaces shows, and every call is logged in order.
private actor FakeReviewAPI: SpacesReviewAPI {
    struct Draft {
        var json: String
        var problems: [SpacesProblemInfo]
        var byOperator: Bool
    }

    var drafts: [Int64: Draft]
    var newest: Int64
    var calls: [String] = []
    var applies: [(Int64, Bool, [String])] = []
    var failApply: (any Error)?
    var failDiff: (any Error)?
    var failSave: (any Error)?
    var orgs: [String] = []

    init(draft: Draft, revision: Int64 = 8) {
        drafts = [revision: draft]
        newest = revision
    }

    func set(failApply: (any Error)? = nil, failDiff: (any Error)? = nil, failSave: (any Error)? = nil,
             orgs: [String]? = nil) {
        self.failApply = failApply
        self.failDiff = failDiff
        self.failSave = failSave
        if let orgs { self.orgs = orgs }
    }

    /// An agent session saves a newer draft behind the user's back.
    func agentSaves(_ json: String) {
        newest += 1
        drafts[newest] = Draft(json: json, problems: [], byOperator: false)
    }

    func diffSpaces() async throws -> SpacesPolicyDiff {
        calls.append("diff")
        if let failDiff { throw failDiff }
        let d = drafts[newest]!
        return SpacesPolicyDiff(appliedRevision: 7, appliedJSON: applied, draftRevision: newest, draftJSON: d.json,
                                problems: d.problems, draftCreatedBy: "session", draftByOperator: d.byOperator,
                                unconfirmedGitHubOrgs: orgs)
    }

    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        calls.append("save")
        if let failSave { throw failSave }
        newest += 1
        // The cleaned draft is valid once the refused read is gone.
        let problems = policyJSON.contains("globex\"]") ? [readsGlobex] : []
        drafts[newest] = Draft(json: policyJSON, problems: problems, byOperator: true)
        return SavedSpacesDraft(revision: newest, problems: problems)
    }

    func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64 {
        calls.append("apply \(reviewedRevision)")
        if let failApply { throw failApply }
        // orchd applies only the newest draft.
        guard reviewedRevision == newest else {
            throw RPCError(code: .failedPrecondition, message: "revision \(reviewedRevision) is not the newest draft (\(newest))")
        }
        applies.append((reviewedRevision, acceptAgentDraft, confirmGitHubOrgs))
        return reviewedRevision
    }

    func listSpaces() async throws -> SpacesListing {
        SpacesListing(status: SpacesPolicyStatus(configured: true, appliedRevision: 7, draftRevision: newest, drift: ["draft"]))
    }
}

/// Records each confirmation it is asked for into the API's call log, so
/// the order of authentication and apply can be checked.
private struct FakeAuthenticator: OperatorAuthenticator {
    let api: FakeReviewAPI
    var succeeds = true

    func confirm(reason: String) async throws {
        await api.log("auth: \(reason)")
        if !succeeds { throw CancellationError() }
    }
}

extension FakeReviewAPI {
    func log(_ s: String) { calls.append(s) }
}

@MainActor
@Suite("Spaces Review model: drop and apply")
struct SpacesReviewModelTests {
    @Test("invalid lines are dropped as a new draft, and Apply sends the revision reviewed")
    func dropThenApplyReviewed() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [readsGlobex], byOperator: true))
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api), home: home)
        await model.load()
        #expect(model.review?.reviewedRevision == 8)
        #expect(model.review?.invalidLines.map(\.text) == ["reads: globex"])
        #expect(model.drift == ["draft"])
        #expect(!model.canApply)
        #expect(model.canDrop)

        // Apply with problems never reaches orchd.
        await model.apply()
        #expect(await api.calls == ["diff"])

        await model.dropInvalidLines()
        #expect(model.notice == "Saved rev 9 without the invalid lines. Review it, then apply it.")
        #expect(model.review?.reviewedRevision == 9)
        #expect(model.review?.hasProblems == false)
        #expect(model.canApply)
        let saved = try #require(await api.drafts[9]?.json)
        #expect(!saved.contains("globex\"]"))

        await model.apply()
        #expect(await api.calls == ["diff", "save", "diff", "auth: apply spaces policy revision 9", "apply 9", "diff"])
        let applied = await api.applies
        #expect(applied.count == 1 && applied[0].0 == 9 && applied[0].1 == false && applied[0].2.isEmpty)
        #expect(model.notice == "Applied rev 9.")
        #expect(model.lastError == nil)
    }

    @Test("a newer draft saved after the review is refused, not applied in its place")
    func staleReviewRefused() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [], byOperator: true))
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api), home: home)
        await model.load()
        await api.agentSaves(applied)
        await model.apply()
        #expect(await api.applies.isEmpty)
        #expect(await api.calls.contains("apply 8"))
        #expect(model.lastError == "revision 8 is not the newest draft (9)")
        // The page now shows the agent's draft, which needs acceptance.
        #expect(model.review?.reviewedRevision == 9)
        #expect(model.needsAgentAcceptance)
    }

    @Test("Apply asks for system authentication first and calls nothing when it is cancelled")
    func authenticationGatesApply() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [], byOperator: true))
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api, succeeds: false), home: home)
        await model.load()
        #expect(model.canApply)
        await model.apply()
        #expect(await api.calls == ["diff", "auth: apply spaces policy revision 8"])
        #expect(await api.applies.isEmpty)
        #expect(model.lastError == "Not applied: authentication was cancelled or failed.")
        #expect(!model.isWorking)
    }

    @Test("an agent's draft needs the user's acceptance before lines are dropped from it or it is applied")
    func agentDraftNeedsAcceptance() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [readsGlobex], byOperator: false))
        await api.set(orgs: ["globex", "acme"])
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api), home: home)
        await model.load()
        #expect(model.needsAgentAcceptance)
        // Dropping makes the rest of the agent's draft the user's own
        // revision, so it waits for the user's acceptance.
        #expect(!model.canDrop)
        await model.dropInvalidLines()
        #expect(await api.calls == ["diff"])
        model.acceptAgentDraft = true
        #expect(model.canDrop)
        await model.dropInvalidLines()
        #expect(model.review?.reviewedRevision == 9)
        // Revision 9 is the operator's own save, accepted before it was
        // made: orchd needs no accept_agent_draft for it.
        #expect(!model.needsAgentAcceptance)
        #expect(!model.acceptAgentDraft)
        model.confirmOrgs = ["globex", "other"]
        #expect(model.canApply)
        await model.apply()
        let applied = await api.applies
        // Only orgs orchd listed are confirmed.
        #expect(applied.count == 1 && applied[0].0 == 9 && applied[0].1 == false && applied[0].2 == ["globex"])
    }

    @Test("an agent's draft applied as is sends accept_agent_draft")
    func agentDraftAccepted() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [], byOperator: false))
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api), home: home)
        await model.load()
        #expect(!model.canApply)
        model.acceptAgentDraft = true
        await model.apply()
        let applied = await api.applies
        #expect(applied.count == 1 && applied[0].1 == true)
        // A new revision resets the acceptance.
        await api.agentSaves(draft)
        await model.load()
        #expect(!model.acceptAgentDraft)
    }

    @Test("orchd's refusals are shown")
    func refusals() async throws {
        let api = FakeReviewAPI(draft: .init(json: draft, problems: [readsGlobex], byOperator: true))
        let model = SpacesReviewModel(api: api, authenticator: FakeAuthenticator(api: api), home: home)
        await api.set(failDiff: RPCError(code: .permissionDenied, message: "only the operator can diff"))
        await model.load()
        #expect(model.lastError == "only the operator can diff")
        #expect(model.review == nil)
        await api.set(failSave: RPCError(code: .permissionDenied, message: "queued tasks cannot save drafts"))
        await model.load()
        await model.dropInvalidLines()
        #expect(model.lastError == "queued tasks cannot save drafts")
        await api.set()
        await model.dropInvalidLines()
        await api.set(failApply: RPCError(code: .permissionDenied, message: "only the operator can apply"))
        await model.apply()
        #expect(model.lastError == "only the operator can apply")
    }
}

@Suite("DaemonClient: Review and Activity RPCs")
struct SpacesReviewClientTests {
    @Test("maps DiffSpaces, SetSpacesDraft, ApplySpaces and ListToolEvents")
    func mapsRPCs() async throws {
        var d = Orchd_V1_SpacesDiff()
        d.appliedRevision = 7
        d.appliedJson = applied
        d.draftRevision = 8
        d.draftJson = draft
        var p = Orchd_V1_SpacesProblem()
        p.rule = readsGlobex.rule
        p.space = readsGlobex.space
        p.message = readsGlobex.message
        d.problems = [p]
        d.draftCreatedBy = "claude session"
        d.draftCreatedAt = 1_800_000_000
        d.draftByOperator = false
        d.unconfirmedGithubOrgs = ["globex"]
        var saved = Orchd_V1_SpacesDraft()
        saved.revision = 9
        saved.problems = [p]
        var applyReply = Orchd_V1_ApplySpacesResponse()
        applyReply.appliedRevision = 9
        var events = Orchd_V1_ListToolEventsResponse()
        var e = Orchd_V1_ToolEvent()
        e.seq = 3
        e.at = 1_800_000_000_500_000_000
        e.kind = "event"
        e.event = "cwd-changed"
        e.tool = "Bash"
        e.agentType = "Explore"
        e.cwd = "/w/a"
        e.oldCwd = "/w/a"
        e.newCwd = "/w/b"
        e.command = "cd /w/b"
        e.targets = "[]"
        e.decision = "logged"
        e.rule = "would refuse (other_job)"
        e.dropped = 0
        events.events = [e]
        events.more = true
        let service = FakeReviewService(diff: d, saved: saved, apply: applyReply, events: events)
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let got = try await client.diffSpaces()
            #expect(got == SpacesPolicyDiff(appliedRevision: 7, appliedJSON: applied, draftRevision: 8, draftJSON: draft,
                                            problems: [readsGlobex], draftCreatedBy: "claude session",
                                            draftCreatedAt: Date(timeIntervalSince1970: 1_800_000_000),
                                            draftByOperator: false, unconfirmedGitHubOrgs: ["globex"]))
            #expect(try await client.saveSpacesDraft(policyJSON: "{}") == SavedSpacesDraft(revision: 9, problems: [readsGlobex]))
            #expect(service.recorder.last(Orchd_V1_SetSpacesDraftRequest.self)?.policyJson == "{}")
            #expect(try await client.applySpaces(reviewedRevision: 9, acceptAgentDraft: true, confirmGitHubOrgs: ["globex"]) == 9)
            let a = service.recorder.last(Orchd_V1_ApplySpacesRequest.self)
            #expect(a?.reviewedRevision == 9 && a?.acceptAgentDraft == true && a?.confirmGithubOrgs == ["globex"])
            let page = try await client.toolEvents(owner: "abc", afterSeq: 2, limit: 50)
            let r = service.recorder.last(Orchd_V1_ListToolEventsRequest.self)
            #expect(r?.owner == "abc" && r?.afterSeq == 2 && r?.limit == 50)
            #expect(page == ToolLogPage(events: [ToolLogEvent(seq: 3, at: Date(timeIntervalSince1970: 1_800_000_000.5),
                                                              kind: "event", event: "cwd-changed", tool: "Bash",
                                                              agentType: "Explore", cwd: "/w/a", oldCwd: "/w/a", newCwd: "/w/b",
                                                              command: "cd /w/b", targets: "[]", decision: "logged",
                                                              rule: "would refuse (other_job)")],
                                        more: true))
        }
    }
}

/// The four Review and Activity RPCs, served like orchd.
private struct FakeReviewService: RegistrableRPCService {
    let diff: Orchd_V1_SpacesDiff
    let saved: Orchd_V1_SpacesDraft
    let apply: Orchd_V1_ApplySpacesResponse
    let events: Orchd_V1_ListToolEventsResponse
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let diff = diff, saved = saved, apply = apply, events = events, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.DiffSpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_DiffSpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpacesDiff>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: diff))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SetSpacesDraft.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SetSpacesDraftRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpacesDraft>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: saved))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ApplySpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ApplySpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ApplySpacesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: apply))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListToolEvents.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListToolEventsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListToolEventsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: events))
        }
    }
}
