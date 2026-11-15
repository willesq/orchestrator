import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

/// Answers the direct-message search like orchd, counting the calls.
private final class FakeDirectMessages: DirectMessagesAPI, @unchecked Sendable {
    private let lock = NSLock()
    var answer: Result<DirectMessageResult, any Error>
    private(set) var queries: [SearchQuery] = []

    init(_ answer: Result<DirectMessageResult, any Error>) { self.answer = answer }

    var calls: Int { lock.withLock { queries.count } }

    func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult {
        try lock.withLock {
            queries.append(query)
            return try answer.get()
        }
    }
}

/// Screen-share-safe mode as the Spaces pane holds it.
@MainActor
private final class Sharing {
    var on = false
}

private let acmeDM = DirectMessageHit(index: 0, text: "the merger codename is bluefin", sourceURI: "slack://TACME/D0001",
                                      tenant: "slack:TACME", space: "acme", score: 0.9)
private let globexDM = DirectMessageHit(index: 1, text: "globex standup", sourceURI: "teams://globex-tenant/19:dm",
                                        tenant: "teams:globex-tenant", space: "globex", score: 0.5)

@MainActor
@Suite struct DirectMessageSearchModelTests {
    /// SP-083.1 a1: the search returns each result with its tenant and
    /// space, and asks only for chat.
    @Test func searchShowsEachResultsTenantAndSpace() async {
        let api = FakeDirectMessages(.success(DirectMessageResult(hits: [acmeDM, globexDM])))
        let model = DirectMessageSearchModel(api: api, isSharing: { false })
        model.query.text = "  bluefin  "
        await model.search()
        #expect(api.calls == 1)
        #expect(api.queries.first?.text == "bluefin")
        #expect(api.queries.first?.sourceType == "chat")
        #expect(model.hits.map(\.tenant) == ["slack:TACME", "teams:globex-tenant"])
        #expect(model.hits.map(\.space) == ["acme", "globex"])
        #expect(model.hits.map(\.spaceLabel) == ["acme", "globex"])
        #expect(model.failure == nil && model.hasSearched && !model.isSearching)
    }

    /// SP-083.1 a1 (spaces off, #209): a hit with no space is labelled as
    /// such, and the search still works.
    @Test func worksWithSpacesOff() async {
        let off = DirectMessageHit(text: "hi", sourceURI: "slack://TACME/D0001", tenant: "slack:TACME", space: "")
        let model = DirectMessageSearchModel(api: FakeDirectMessages(.success(DirectMessageResult(hits: [off]))),
                                             isSharing: { false })
        model.query.text = "hi"
        await model.search()
        #expect(model.hits.map(\.spaceLabel) == ["No space"])
        #expect(model.hits.map(\.tenantLabel) == ["slack:TACME"])
        #expect(DirectMessageHit(text: "x", sourceURI: "u").tenantLabel == "Unknown tenant")
    }

    /// SP-083.1 a1: while screen-share-safe mode is on no search is sent,
    /// held results are hidden at once and dropped, and the view says why.
    @Test func screenShareSafeModeShowsNoDirectMessage() async {
        let sharing = Sharing()
        let api = FakeDirectMessages(.success(DirectMessageResult(hits: [acmeDM], fallback: "dense only")))
        let model = DirectMessageSearchModel(api: api, isSharing: { sharing.on })
        model.query.text = "bluefin"
        await model.search()
        #expect(model.hits == [acmeDM])

        sharing.on = true
        #expect(model.hits.isEmpty, "held results hide the moment sharing starts")
        #expect(model.fallback == nil && !model.hasSearched)
        #expect(model.failure == .sharing)
        await model.search()
        #expect(api.calls == 1, "no search is sent while sharing")
        #expect(model.hits.isEmpty)

        // A search tried while sharing drops what was held, so nothing
        // comes back when sharing ends.
        sharing.on = false
        #expect(model.hits.isEmpty && !model.hasSearched && model.failure == nil)
    }

    /// A search that answers after sharing started keeps nothing.
    @Test func resultsArrivingWhileSharingAreDropped() async {
        let sharing = Sharing()
        let api = SharingStarter(sharing: sharing)
        let model = DirectMessageSearchModel(api: api, isSharing: { sharing.on })
        model.query.text = "bluefin"
        await model.search()
        sharing.on = false
        #expect(model.hits.isEmpty && !model.hasSearched)
    }

    /// Empty text sends nothing.
    @Test func emptyQuerySendsNothing() async {
        let api = FakeDirectMessages(.success(DirectMessageResult(hits: [acmeDM])))
        let model = DirectMessageSearchModel(api: api, isSharing: { false })
        model.query.text = "   "
        await model.search()
        #expect(api.calls == 0 && !model.hasSearched)
    }

    /// SP-083.1 a2: a refused search says orchd refused it and who may
    /// search direct messages; it shows no results.
    @Test func refusedSearchSaysWhy() async {
        let api = FakeDirectMessages(.failure(RPCError(code: .permissionDenied,
                                                       message: "only the operator can search direct messages")))
        let model = DirectMessageSearchModel(api: api, isSharing: { false })
        model.query.text = "bluefin"
        await model.search()
        let failure = try? #require(model.failure)
        #expect(failure?.kind == .refused)
        #expect(failure?.title == "orchd refused the search")
        #expect(failure?.detail.contains("only the operator can search direct messages") == true)
        #expect(failure?.detail.contains("Orch.app") == true)
        #expect(model.hits.isEmpty && !model.hasSearched)
    }

    /// SP-083.1 a2: a failed search says what failed, for each kind of
    /// failure orchd reports.
    @Test func failedSearchSaysWhy() async {
        let cases: [(any Error, String, String)] = [
            (RPCError(code: .invalidArgument, message: "top_k 500 must be between 0 and 100"),
             "The search could not run", "top_k 500 must be between 0 and 100"),
            (RPCError(code: .failedPrecondition, message: "hybrid search is unavailable (see orchd log)"),
             "The search could not run", "hybrid search is unavailable"),
            (RPCError(code: .unimplemented, message: ""), "This orchd cannot search direct messages", "newer orchd"),
            (RPCError(code: .unavailable, message: "connection refused"), "The search failed", "unavailable"),
            (RPCError(code: .unauthenticated, message: ""), "orchd refused the search", "Only the operator"),
        ]
        for (error, title, detail) in cases {
            let model = DirectMessageSearchModel(api: FakeDirectMessages(.failure(error)), isSharing: { false })
            model.query.text = "bluefin"
            await model.search()
            #expect(model.failure?.title == title, "\(error)")
            #expect(model.failure?.detail.contains(detail) == true, "\(String(describing: model.failure?.detail))")
        }
        // A failure is replaced by the next search's results.
        let api = FakeDirectMessages(.failure(RPCError(code: .unavailable, message: "")))
        let model = DirectMessageSearchModel(api: api, isSharing: { false })
        model.query.text = "bluefin"
        await model.search()
        #expect(model.failure != nil)
        api.answer = .success(DirectMessageResult(hits: [acmeDM]))
        await model.search()
        #expect(model.failure == nil && model.hits == [acmeDM])
    }

    /// orchd's message is made safe to show: no control characters.
    @Test func failureTextIsDisplaySafe() async {
        for code in [RPCError.Code.internalError, .invalidArgument] {
            let api = FakeDirectMessages(.failure(RPCError(code: code, message: code == .invalidArgument ? "" : "bad\u{202E}gnirts")))
            let model = DirectMessageSearchModel(api: api, isSharing: { false })
            model.query.text = "x"
            await model.search()
            #expect(model.failure?.detail.contains("\u{202E}") == false, "\(code)")
        }
    }

    @Test func refusalMessageIsDisplaySafe() async {
        let api = FakeDirectMessages(.failure(RPCError(code: .permissionDenied, message: "no\u{202E}pe")))
        let model = DirectMessageSearchModel(api: api, isSharing: { false })
        model.query.text = "x"
        await model.search()
        #expect(model.failure?.detail.contains("\u{202E}") == false)
    }
}

/// Answers, but turns sharing on before it does.
private final class SharingStarter: DirectMessagesAPI, @unchecked Sendable {
    let sharing: Sharing
    init(sharing: Sharing) { self.sharing = sharing }

    func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult {
        await MainActor.run { sharing.on = true }
        return DirectMessageResult(hits: [acmeDM])
    }
}

@Suite struct DirectMessagesClientTests {
    /// SP-083.1 a1: the app's client sends SearchDirectMessages (never the
    /// plain Search, which leaves DMs out) with the query, and returns each
    /// hit's tenant and space as orchd sent them.
    @Test func callsSearchDirectMessagesAndKeepsTenantAndSpace() async throws {
        var response = Orchd_V1_SearchResponse()
        var hit = Orchd_V1_SearchHit()
        hit.text = "the merger\u{202E} codename"
        hit.sourceUri = "slack://TACME/D0001"
        hit.tenant = "slack:TACME"
        hit.space = "acme"
        hit.score = 0.75
        hit.startedAt = 1_700_000_000
        var plain = Orchd_V1_SearchHit()
        plain.sourceUri = "teams://g/19:dm"
        response.hits = [hit, plain]
        response.fallback = "dense only"
        let service = FakeDirectMessagesService(response: response)
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let got = try await client.searchDirectMessages(SearchQuery(text: " bluefin ", limit: 20, sourceType: "chat"))
            let sent = try #require(service.recorder.last(Orchd_V1_SearchDirectMessagesRequest.self))
            #expect(sent.search.query == "bluefin")
            #expect(sent.search.topK == 20)
            #expect(sent.search.sourceType == "chat")
            #expect(got.fallback == "dense only")
            #expect(got.hits.count == 2)
            #expect(got.hits[0].tenant == "slack:TACME" && got.hits[0].space == "acme")
            #expect(got.hits[0].sourceURI == "slack://TACME/D0001" && got.hits[0].score == 0.75)
            #expect(got.hits[0].startedAt == Date(timeIntervalSince1970: 1_700_000_000))
            #expect(!got.hits[0].text.contains("\u{202E}"), "chat text is made display-safe")
            #expect(got.hits[1].tenant.isEmpty && got.hits[1].space.isEmpty && got.hits[1].startedAt == nil)
            #expect(got.hits[0].id != got.hits[1].id)
        }
    }

    /// SP-083.1 a2: orchd's refusal reaches the model and is explained.
    @Test func refusalReachesTheModel() async throws {
        let service = FakeDirectMessagesService(refusal: RPCError(code: .permissionDenied,
                                                                  message: "only the operator can search direct messages"))
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let model = await DirectMessageSearchModel(api: client, isSharing: { false })
            await MainActor.run { model.query.text = "bluefin" }
            await model.search()
            let failure = await model.failure
            #expect(failure?.kind == .refused)
            #expect(failure?.detail.hasSuffix("orchd said: \u{201C}only the operator can search direct messages\u{201D}") == true)
        }
    }
}

/// SearchDirectMessages served like orchd.
private struct FakeDirectMessagesService: RegistrableRPCService {
    var response = Orchd_V1_SearchResponse()
    var refusal: RPCError?
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let response = response, refusal = refusal, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SearchDirectMessages.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SearchDirectMessagesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SearchResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            if let refusal { return StreamingServerResponse(single: ServerResponse(error: refusal)) }
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
    }
}
