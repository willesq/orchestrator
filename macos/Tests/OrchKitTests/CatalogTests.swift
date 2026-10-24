import Foundation
import Testing
@testable import OrchKit

private struct Down: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private final class FakeCatalog: CatalogAPI, MemoryAPI, @unchecked Sendable {
    private let lock = NSLock()
    var catalog = DocCatalog(owners: [], files: [])
    var documents: [String: RebuiltDocument] = [:]
    var sources: [RegisteredSource] = []
    var fail = false
    var failDocument = false
    var documentDelay: Duration = .zero
    private(set) var searches: [SearchQuery] = []
    private(set) var saved: [(SourceDraft, String?)] = []
    private(set) var removed: [(String, Bool)] = []
    private(set) var refreshed: [String?] = []

    func listDocs() async throws -> DocCatalog {
        if fail { throw Down() }
        return catalog
    }

    func document(uri: String) async throws -> RebuiltDocument {
        try await Task.sleep(for: documentDelay)
        if failDocument { throw Down() }
        return documents[uri] ?? RebuiltDocument(sourceURI: uri, text: "")
    }

    func registeredSources() async throws -> [RegisteredSource] {
        if fail { throw Down() }
        return sources
    }

    func saveSource(_ draft: SourceDraft, replacing: String?) async throws -> RegisteredSource {
        if fail { throw Down() }
        lock.withLock { saved.append((draft, replacing)) }
        let source = RegisteredSource(id: "\(draft.kind.rawValue)|\(draft.trimmedURI)", kind: draft.kind, uri: draft.trimmedURI)
        sources.removeAll { $0.id == source.id || $0.id == replacing }
        sources.append(source)
        return source
    }

    func removeSource(id: String, purge: Bool) async throws -> PurgeResult {
        if fail { throw Down() }
        lock.withLock { removed.append((id, purge)) }
        sources.removeAll { $0.id == id }
        return PurgeResult(sources: purge ? 2 : 0, chunks: purge ? 7 : 0)
    }

    func refreshSource(id: String?) async throws {
        if fail { throw Down() }
        lock.withLock { refreshed.append(id) }
    }

    func search(_ query: SearchQuery) async throws -> SearchResult {
        lock.withLock { searches.append(query) }
        return SearchResult(hits: [], fallback: nil)
    }

    func listSources(type: String, limit: Int) async throws -> SourceList { SourceList(sources: [], totalSources: 0, totalChunks: 0) }
}

private func file(_ owner: String, _ repo: String, _ path: String) -> DocFile {
    DocFile(sourceURI: "repo://\(owner)/\(repo)/\(path)", owner: owner, repo: repo, path: path,
            webURL: "https://github.com/\(owner)/\(repo)/blob/HEAD/\(path)", chunks: 2)
}

@MainActor
@Suite struct DocsModelTests {
    private func loaded() async -> (DocsModel, FakeCatalog) {
        let api = FakeCatalog()
        api.catalog = DocCatalog(
            owners: [DocOwner(owner: "willesq", repos: [DocRepo(repo: "orchestrator", headSHA: "abc", files: 2)])],
            files: [file("willesq", "orchestrator", "README.md"), file("willesq", "orchestrator", "docs/INSTALL.md"),
                    file("Acme", "api", "README.md")]
        )
        api.documents["repo://willesq/orchestrator/README.md"] = RebuiltDocument(sourceURI: "repo://willesq/orchestrator/README.md",
                                                                                 text: "# Orch", chunks: 2)
        let model = DocsModel(api: api)
        await model.load()
        return (model, api)
    }

    @Test func groupsAndFilters() async {
        let (model, _) = await loaded()
        #expect(model.groups.map(\.id) == ["Acme/api", "willesq/orchestrator"])
        #expect(model.groups[1].files.map(\.path) == ["docs/INSTALL.md", "README.md"])
        #expect(model.groups[1].info?.headSHA == "abc")
        #expect(model.groups[0].info == nil)
        #expect(model.fileCount == 3)
        #expect(model.chunkCount == 6)
        model.filter = " install "
        #expect(model.groups.map(\.id) == ["willesq/orchestrator"])
        model.filter = "acme/"
        #expect(model.fileCount == 1)
    }

    @Test func selectingRebuildsAndScopesSearch() async {
        let (model, api) = await loaded()
        let readme = model.groups[1].files[1]
        model.search.query.text = "install"
        await model.select(readme)
        #expect(model.document?.text == "# Orch")
        #expect(model.documentError == nil && !model.isLoadingDocument)
        #expect(model.search.query.sourceURI == readme.sourceURI)
        #expect(model.search.query.text == "install")
        await model.search.search()
        #expect(api.searches.last?.sourceURI == readme.sourceURI)

        model.setScope(.repo)
        #expect(model.search.query.sourceURI.isEmpty)
        #expect(model.search.query.allTags == ["owner:willesq", "repo:orchestrator"])
        #expect(model.search.result == nil)

        await model.select(nil)
        #expect(model.selected == nil && model.document == nil)
        #expect(model.search.query.allTags.isEmpty)
    }

    @Test func aSlowLoadCannotReplaceANewerOne() async throws {
        let (model, api) = await loaded()
        api.documentDelay = .milliseconds(200)
        let first = Task { await model.select(model.groups[0].files[0]) }
        try await Task.sleep(for: .milliseconds(20))
        api.documentDelay = .zero
        await model.select(model.groups[1].files[1])
        await first.value
        #expect(model.selected?.path == "README.md" && model.selected?.owner == "willesq")
        #expect(model.document?.text == "# Orch")
    }

    @Test func reportsFailures() async {
        let (model, api) = await loaded()
        api.failDocument = true
        await model.select(model.groups[1].files[1])
        #expect(model.documentError == "unavailable" && model.document == nil)
        api.fail = true
        await model.load()
        #expect(model.lastError == "unavailable")
        #expect(model.catalog != nil, "a failed reload keeps the last list")
    }

    @Test func emptyWithoutACatalog() {
        let model = DocsModel(api: FakeCatalog())
        #expect(model.groups.isEmpty && model.fileCount == 0)
    }
}

@MainActor
@Suite struct RegisteredSourcesModelTests {
    @Test func savesEditsRefreshesAndRemoves() async {
        let api = FakeCatalog()
        let model = RegisteredSourcesModel(api: api)
        await model.load()
        #expect(model.loaded && model.sources.isEmpty)

        #expect(await model.save(SourceDraft(kind: .web, uri: " https://go.dev/doc ", tags: "go, docs, go")))
        #expect(model.selected?.uri == "https://go.dev/doc")
        #expect(model.message?.contains("Added") == true)
        #expect(api.saved.last?.0.tagList == ["go", "docs"])

        let original = model.selected!
        var draft = original.draft
        draft.uri = "https://go.dev/ref"
        #expect(await model.save(draft, editing: original))
        #expect(api.saved.last?.1 == original.id)
        #expect(model.sources.map(\.uri) == ["https://go.dev/ref"])
        #expect(model.message?.hasPrefix("Saved") == true)

        await model.refresh(model.selected)
        await model.refresh(nil)
        #expect(api.refreshed == ["web|https://go.dev/ref", nil])
        #expect(model.message == "Refreshing every source now.")

        #expect(await model.remove(model.selected!, purge: true))
        #expect(api.removed.last! == ("web|https://go.dev/ref", true))
        #expect(model.message?.contains("deleted 2 indexed sources (7 chunks)") == true)
        #expect(model.selected == nil, "a removed source is deselected")
    }

    @Test func removingWithoutPurgeSaysTheContentStays() async {
        let api = FakeCatalog()
        api.sources = [RegisteredSource(id: "owner|acme", kind: .owner, uri: "acme")]
        let model = RegisteredSourcesModel(api: api)
        await model.load()
        #expect(await model.remove(api.sources[0], purge: false))
        #expect(model.message?.contains("still in memory") == true)
    }

    @Test func aDraftWithAProblemIsNotSent() async {
        let api = FakeCatalog()
        let model = RegisteredSourcesModel(api: api)
        #expect(!(await model.save(SourceDraft(kind: .owner, uri: "../etc"))))
        #expect(api.saved.isEmpty && model.lastError != nil)
    }

    @Test func selectingScopesTheSearch() async {
        let api = FakeCatalog()
        api.sources = [
            RegisteredSource(id: "web|u", kind: .web, uri: "u", searchSourceURI: "u"),
            RegisteredSource(id: "owner|acme", kind: .owner, uri: "acme", searchAllTags: ["owner:acme"]),
        ]
        let model = RegisteredSourcesModel(api: api)
        await model.load()
        model.select("web|u")
        #expect(model.search.query.sourceURI == "u" && model.search.query.allTags.isEmpty)
        model.select("owner|acme")
        #expect(model.search.query.sourceURI.isEmpty && model.search.query.allTags == ["owner:acme"])
        model.select(nil)
        #expect(model.selected == nil && model.search.query.allTags.isEmpty)
    }

    @Test func reportsFailures() async {
        let api = FakeCatalog()
        api.fail = true
        let model = RegisteredSourcesModel(api: api)
        await model.load()
        #expect(model.lastError == "unavailable" && !model.loaded)
        #expect(!(await model.save(SourceDraft(kind: .web, uri: "https://x.dev"))))
        #expect(!(await model.remove(RegisteredSource(id: "x", kind: .web, uri: "x"), purge: false)))
        await model.refresh(nil)
        #expect(model.lastError == "unavailable" && !model.busy)
    }
}

@Suite struct SourceDraftTests {
    @Test(arguments: [
        (SourceKind.web, "https://go.dev/doc", nil as String?),
        (.web, "ftp://x", "http or https"),
        (.web, "https://me:pw@x.dev", "credentials"),
        (.web, "not a url", "http or https"),
        (.repo, "/Users/me/code/app", nil),
        (.repo, "code/app", "full path"),
        (.owner, "willesq", nil),
        (.owner, "a/b", "one folder"),
        (.owner, "..", "one folder"),
        (.owner, "local", "reserved"),
    ])
    func addressRules(kind: SourceKind, uri: String, problem: String?) {
        let found = SourceDraft(kind: kind, uri: uri).problem
        if let problem {
            #expect(found?.contains(problem) == true, "\(uri): \(found ?? "nil")")
        } else {
            #expect(found == nil, "\(uri): \(found ?? "")")
        }
    }

    @Test func fieldLimits() {
        #expect(SourceDraft(kind: .owner, uri: "o", scope: String(repeating: "s", count: 65)).problem?.contains("Scope") == true)
        #expect(SourceDraft(kind: .owner, uri: "o", tags: "repo:x").problem?.contains("added automatically") == true)
        #expect(SourceDraft(kind: .owner, uri: "o", tags: String(repeating: "t", count: 65)).problem?.contains("64") == true)
        let many = (0..<33).map { "t\($0)" }.joined(separator: ",")
        #expect(SourceDraft(kind: .owner, uri: "o", tags: many).problem?.contains("32") == true)
        #expect(SourceDraft(kind: .repo, uri: "/" + String(repeating: "a", count: 2048)).problem?.contains("too long") == true)
    }

    @Test func registeredSourceRoundTripsToADraft() {
        let s = RegisteredSource(id: "web|u", kind: .web, uri: "https://x.dev", scope: "work", tags: ["a", "b"], refreshSeconds: 3600)
        #expect(s.draft == SourceDraft(kind: .web, uri: "https://x.dev", scope: "work", tags: "a, b", refreshSeconds: 3600))
        #expect(!s.isSearchable)
        #expect(RegisteredSource(id: "x", kind: nil, uri: "x").draft.kind == .web)
        #expect(Set(SourceKind.allCases.map(\.label)).count == 3)
        #expect(SourceKind.allCases.allSatisfy { !$0.placeholder.isEmpty })
        #expect(file("o", "r", "p").repoTags == ["owner:o", "repo:r"])
    }
}

@Suite struct CatalogURLTests {
    @Test(arguments: [
        ("https://github.com/o/r/blob/abc/README.md", true),
        ("https://github.com/o/r/blob/abc/docs/a%20b.md", true),
        ("https://github.com/o/r/tree/abc/docs", false),
        ("https://github.com/o/r/blob/abc", false),
        ("https://github.com/o/r/blob/abc/../x", false),
        ("https://github.com/o/r/blob/abc/x?raw=1", false),
        ("https://github.com:8443/o/r/blob/abc/x", false),
        ("http://github.com/o/r/blob/abc/x", false),
        ("https://evil.example/o/r/blob/abc/x", false),
        ("javascript:alert(1)", false),
    ])
    func gitHubBlob(raw: String, allowed: Bool) {
        #expect((gitHubBlobURL(raw) != nil) == allowed)
    }

    @Test(arguments: [
        ("https://go.dev/doc", true),
        ("http://localhost:8080/x", true),
        ("file:///etc/passwd", false),
        ("https://me:pw@x.dev", false),
        ("", false),
    ])
    func webSource(raw: String, allowed: Bool) {
        #expect((webSourceURL(raw) != nil) == allowed)
    }
}

@Suite struct CatalogRoundTripTests {
    @Test func mapsEveryCall() async throws {
        var service = FakeOrchdService()
        var repo = Orchd_V1_DocRepo()
        repo.repo = "orchestrator"
        repo.headSha = "abc"
        repo.files = 2
        repo.lastSynced = 1_700_000_000
        var owner = Orchd_V1_DocOwner()
        owner.owner = "willesq"
        owner.refreshSecs = 3600
        owner.repos = [repo]
        var docFile = Orchd_V1_DocFile()
        docFile.sourceUri = "repo://willesq/orchestrator/README.md"
        docFile.owner = "willesq"
        docFile.repo = "orchestrator"
        docFile.path = "README.md"
        docFile.updatedAt = 1_700_000_100
        docFile.webURL = "https://github.com/willesq/orchestrator/blob/abc/README.md"
        docFile.chunks = 4
        service.docs.owners = [owner]
        service.docs.files = [docFile]
        service.docs.truncated = true
        service.document.sourceUri = docFile.sourceUri
        service.document.sourceType = "repo_doc"
        service.document.text = "# Orch"
        service.document.chunks = 3
        service.document.malformed = true
        var src = Orchd_V1_RegisteredSource()
        src.id = "owner|acme"
        src.kind = "owner"
        src.uri = "acme"
        src.searchAllTags = ["owner:acme"]
        src.lastFetched = 0
        service.registered.sources = [src]

        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let catalog = try await client.listDocs()
            #expect(catalog.truncated)
            #expect(catalog.owners == [DocOwner(owner: "willesq", refreshSeconds: 3600, status: "",
                                                repos: [DocRepo(repo: "orchestrator", headSHA: "abc",
                                                                lastSynced: Date(timeIntervalSince1970: 1_700_000_000), files: 2)])])
            #expect(catalog.files.first?.webURL == docFile.webURL && catalog.files.first?.updatedAt == Date(timeIntervalSince1970: 1_700_000_100))
            #expect(catalog.files.first?.chunks == 4)

            let doc = try await client.document(uri: docFile.sourceUri)
            #expect(doc == RebuiltDocument(sourceURI: docFile.sourceUri, sourceType: "repo_doc", text: "# Orch", chunks: 3, malformed: true))
            #expect(service.recorder.last(Orchd_V1_GetDocumentRequest.self)?.sourceUri == docFile.sourceUri)

            let sources = try await client.registeredSources()
            #expect(sources == [RegisteredSource(id: "owner|acme", kind: .owner, uri: "acme", refreshSeconds: 0, status: "",
                                                 searchAllTags: ["owner:acme"])])

            let saved = try await client.saveSource(SourceDraft(kind: .web, uri: " https://go.dev ", scope: " work ", tags: "a, b",
                                                                refreshSeconds: 3600), replacing: "web|old")
            #expect(saved.id == "web|https://go.dev" && saved.status == "new")
            let save = service.recorder.last(Orchd_V1_SaveSourceRequest.self)
            #expect(save?.replaceID == "web|old" && save?.source.scope == "work" && save?.source.tags == ["a", "b"]
                && save?.source.refreshSecs == 3600)

            let purged = try await client.removeSource(id: "owner|acme", purge: true)
            #expect(purged == PurgeResult(sources: 2, chunks: 9))
            let remove = service.recorder.last(Orchd_V1_RemoveSourceRequest.self)
            #expect(remove?.purge == true && remove?.confirm == true)
            _ = try await client.removeSource(id: "owner|acme", purge: false)
            #expect(service.recorder.last(Orchd_V1_RemoveSourceRequest.self)?.confirm == false)

            try await client.refreshSource(id: nil)
            #expect(service.recorder.last(Orchd_V1_RefreshSourceRequest.self)?.id == "")

            _ = try await client.search(SearchQuery(text: "q", sourceURI: "u", allTags: ["owner:o", ""]))
            let search = service.recorder.last(Orchd_V1_SearchRequest.self)
            #expect(search?.sourceUri == "u" && search?.allTags == ["owner:o"])
        }
    }
}
