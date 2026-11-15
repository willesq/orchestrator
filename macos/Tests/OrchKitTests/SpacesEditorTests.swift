import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private let home = "/Users/fixture"

private func spaces(_ json: String = editorFixturePolicy) -> [EditableSpace] { SpacesPolicyEdit(json: json).spaces }

/// SP-053.1.a1: the reads matrix's locked cells.
@Suite struct SpacesAccessTests {
    /// Every cell of the five-space fixture, against orchd's rules
    /// (validate.go): a locked cell cannot be switched on through the model,
    /// and an isolated reader's cell on a private space is switchable with a
    /// warning.
    @Test @MainActor func lockedCellsCannotBeEnabledThroughTheUIModel() async {
        let api = FakeEditorAPI()
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        let kinds = Dictionary(uniqueKeysWithValues: model.spaces.map { ($0.name, $0.kind) })
        var locked = 0, warned = 0
        for row in model.matrix.rows {
            for cell in row {
                let r = kinds[cell.reader]!, t = kinds[cell.target]!
                let before = model.edit.json
                switch cell.state {
                case .itself:
                    #expect(cell.reader == cell.target)
                case .always:
                    #expect(t == "public" && cell.reads)
                case .locked:
                    locked += 1
                    // orchd refuses exactly these: anything to isolated, and a public reader.
                    #expect(t == "isolated" || r == "public", "\(cell.reader)→\(cell.target)")
                    #expect(!cell.canEnable && !cell.reads)
                    #expect(model.setRead(reader: cell.reader, target: cell.target, on: true) == false)
                    #expect(model.accessError != nil)
                    #expect(model.edit.json == before, "a locked cell changed the draft")
                    if case .failure = model.matrix.reads(after: cell.reader, target: cell.target, on: true) {} else {
                        Issue.record("the matrix let \(cell.reader)→\(cell.target) be enabled")
                    }
                case .switchable(let warning):
                    #expect(t == "private" && r != "public")
                    #expect((warning != nil) == (r == "isolated"), "\(cell.reader)→\(cell.target)")
                    if warning != nil { warned += 1 }
                }
            }
        }
        // isolated→isolated (acme↔globex), private→isolated (2×2), public→private/isolated (4).
        #expect(locked == 2 + 4 + 4)
        #expect(warned == 4)

        // Isolated to private is switchable; it lands in the draft's reads.
        let cell = try! #require(model.matrix.cell(reader: "acme", target: "personal"))
        guard case .switchable(let warning?) = cell.state else { Issue.record("acme→personal not switchable with a warning"); return }
        #expect(warning.contains("acme") && warning.contains("personal"))
        #expect(model.setRead(reader: "acme", target: "personal", on: true))
        #expect(model.spaces.first { $0.name == "acme" }?.reads == ["personal"])
        #expect(model.matrix.cell(reader: "acme", target: "personal")?.reads == true)
        #expect(model.setRead(reader: "acme", target: "personal", on: false))
        #expect(model.spaces.first { $0.name == "acme" }?.reads == [])
        // Private to private is switchable without a warning, and starts as the document says.
        #expect(model.matrix.cell(reader: "personal", target: "notes")?.listed == true)
        #expect(model.setRead(reader: "personal", target: "notes", on: false))
        #expect(model.spaces.first { $0.name == "personal" }?.reads == [])
    }

    /// A locked read the document lists anyway is marked, blocks saving and
    /// can only be switched off.
    @Test @MainActor func lockedReadInTheDocumentCanOnlyBeSwitchedOff() async {
        let api = FakeEditorAPI()
        api.diff = SpacesPolicyDiff(appliedRevision: 4, appliedJSON: editorFixturePolicy, draftRevision: 6,
                                    draftJSON: editorFixturePolicy.replacingOccurrences(of: #""github_orgs":["globex"]"#,
                                                                                        with: #""github_orgs":["globex"],"reads":["acme"]"#))
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        let cell = try! #require(model.matrix.cell(reader: "globex", target: "acme"))
        #expect(cell.invalid && cell.canDisable && !cell.canEnable)
        #expect(model.matrix.invalidCells.count == 1)
        model.setValue("name", of: 2, to: "notes2") // make it dirty
        #expect(model.saveBlocker?.contains("not allowed") == true || model.saveBlocker?.contains("may not") == true)
        await model.save()
        #expect(api.saved.isEmpty)
        #expect(model.setRead(reader: "globex", target: "acme", on: false))
        #expect(model.matrix.invalidCells.isEmpty)
        // Always-read cells have nothing to switch.
        #expect(model.setRead(reader: "acme", target: "oss", on: false) == false)
        #expect(model.setRead(reader: "acme", target: "acme", on: true) == false)
        #expect(model.setRead(reader: "nope", target: "acme", on: true) == false)
        // Switching an already-on switchable cell on, or an off one off, changes nothing.
        #expect(model.setRead(reader: "personal", target: "notes2", on: true))
        #expect(model.spaces.first { $0.name == "personal" }?.reads == ["notes2"])
        #expect(model.setRead(reader: "acme", target: "personal", on: false))
        #expect(model.spaces.first { $0.name == "acme" }?.reads == [])
    }

    @Test func unknownKindIsLocked() {
        let m = ReadsMatrix(spaces: spaces(#"{"spaces":[{"name":"a","kind":"private"},{"name":"b","kind":"weird"}]}"#))
        guard case .locked = m.cell(reader: "b", target: "a")?.state else { Issue.record("unknown reader not locked"); return }
        #expect(m.cell(reader: "a", target: "zzz") == nil)
    }

    @Test func contentKindTableNamesWherePublicRootSessionsWrite() {
        let named = ContentKindRule.table(publicRootSessions: "personal")
        #expect(named.count == 6)
        #expect(named.contains { $0.goesTo.contains("writes to personal") })
        let chosen = ContentKindRule.table(publicRootSessions: "")
        #expect(chosen.contains { $0.goesTo.contains("only private space") })
        #expect(Set(chosen.map(\.id)).count == chosen.count)
    }
}

/// SP-053.1.a4: the roots tree marks every nesting orchd refuses, and the
/// editor cannot save it.
@Suite struct SpacesRootsTreeTests {
    @Test @MainActor func rejectedNestingIsMarkedAndCannotBeSubmitted() async {
        let api = FakeEditorAPI()
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(!model.rootsTree.blocksSubmit, "the fixture's nesting is valid")
        #expect(model.rootsTree.nodes.map(\.depth) == [0, 1, 1])
        #expect(model.rootsTree.nodes.allSatisfy { $0.effectiveSpace == $0.space })

        let cases: [(String, Int, [String], String)] = [
            // A public root inside a private one: less strict.
            ("oss's root inside personal's", 0, ["~/code", "~/code/willesq/pub"], "less strict"),
            // A private root inside a job's root: less strict.
            ("personal inside acme", 1, ["~/code/willesq", "~/code/company1/mine"], "less strict than acme"),
            // An isolated root inside another job's root.
            ("globex inside acme", 4, ["~/code/company1/gx"], "inside job acme"),
            // The same folder bound twice, case folded as on macOS.
            ("bound twice", 4, ["~/Code/Company1"], "already bound to acme"),
            ("relative", 4, ["code/x"], "not a full path"),
        ]
        for (name, index, roots, why) in cases {
            model.revert()
            model.setList("roots", of: index, to: roots)
            let tree = model.rootsTree
            #expect(tree.blocksSubmit, "\(name)")
            #expect(tree.nodes.flatMap(\.problems).contains { $0.contains(why) }, "\(name): \(tree.nodes.flatMap(\.problems))")
            #expect(tree.nodes.contains { $0.effectiveSpace == "refused" })
            #expect(model.saveBlocker?.contains("nesting rules") == true, "\(name)")
            await model.save()
            #expect(api.saved.isEmpty, "\(name) was submitted")
        }
        // A stricter root inside a looser one is fine: private inside public, isolated inside private.
        model.revert()
        model.setList("roots", of: 4, to: ["~/code/willesq/globex-work"])
        #expect(!model.rootsTree.blocksSubmit)
        await model.save()
        #expect(api.saved.count == 1)
    }

    @Test func rootFolderHoldsEverything() {
        let tree = RootsTree(spaces: spaces(#"{"spaces":[{"name":"a","kind":"isolated","roots":["/"]},{"name":"b","kind":"isolated","roots":["/x"]}]}"#),
                             home: home)
        #expect(tree.problemCount == 1)
        #expect(tree.nodes.last?.depth == 1)
    }
}

/// Editing keeps the document whole.
@Suite struct SpacesPolicyEditTests {
    @Test func editsKeepUnknownFieldsAndReferences() throws {
        var e = SpacesPolicyEdit(json: editorFixturePolicy)
        e.setList("github_orgs", of: 3, to: [" acme ", "", "acme", "acme-labs"])
        #expect(e.spaces[3].githubOrgs == ["acme", "acme-labs"])
        e.setList("chat", of: 3, to: [])
        #expect(e.spaces[3].chat.isEmpty)
        e.setList("bogus", of: 3, to: ["x"])
        e.setValue("bogus", of: 3, to: "x")
        e.setValue("terms_file", of: 3, to: "")
        #expect(e.spaces[3].termsFile == "")
        e.setValue("name", of: 2, to: "journal")
        #expect(e.spaces[1].reads == ["journal"])
        e.setValue("name", of: 1, to: "me")
        #expect(e.publicRootSessions == "me")
        e.removeSpace(at: 2)
        #expect(e.spaces[1].reads.isEmpty)
        e.removeSpace(at: 1)
        #expect(e.publicRootSessions == "")
        e.removeSpace(at: 99)
        e.addSpace(name: " new ", kind: "private")
        #expect(e.spaces.last?.name == "new" && e.spaces.last?.kind == "private")
        let doc = try #require(JSONValue.parse(e.json))
        #expect(doc["x_top"] != nil)
        #expect(doc["spaces"]?.arrayValue?.first { $0["name"]?.stringValue == "acme" }?["x_future"] == .bool(true))
        #expect(!e.json.contains("\"bogus\""))
    }

    @Test func unreadableDocumentStartsEmpty() {
        #expect(SpacesPolicyEdit(json: "").spaces.isEmpty)
        #expect(SpacesPolicyEdit(json: "[1]").json == #"{"spaces":[]}"#)
        #expect(SpacesPolicyEdit(json: #"{"public_root_sessions":"x"}"#).publicRootSessions == "x")
    }
}

/// The generated rules and their match counts.
@Suite struct SpacesRulesTests {
    @Test func rulesCountTheSourcesTheyName() {
        let sources = [
            "file:///Users/fixture/code/company1/app/README.md", "file:///Users/fixture/code/company1",
            "file:///Users/fixture/code/company10/x", "repo://acme/app/README.md", "repo://ACME/docs/a.md",
            "https://docs.oss.dev/guide", "https://docs.oss.dev.evil/x", "slack://TACME/C1/1", "note://x",
        ]
        let rules = GeneratedRule.rules(spaces: spaces(), sources: sources, home: home)
        func count(_ kind: GeneratedRule.Kind, _ value: String) -> Int? { rules.first { $0.kind == kind && $0.value == value }?.matches }
        #expect(count(.root, "~/code/company1") == 2)
        #expect(count(.root, "~/code") == 3)
        #expect(count(.githubOrg, "acme") == 2)
        #expect(count(.webPrefix, "https://docs.oss.dev/") == 1)
        #expect(count(.chat, "slack:TACME") == 1)
        #expect(rules.first { $0.kind == .publicPath }?.matches == nil)
        // Roots deepest first, then owners, prefixes, tenants and public paths.
        #expect(rules.prefix(3).map(\.kind) == [.root, .root, .root])
        #expect(rules.first?.value != "~/code")
        #expect(rules.last?.kind == .publicPath)
        #expect(GeneratedRule.Kind.allCases.allSatisfy { !$0.title.isEmpty && !$0.effect.isEmpty })
        let bad = GeneratedRule.rules(spaces: spaces(#"{"spaces":[{"name":"a","kind":"private","roots":["rel"],"chat":["nocolon"]}]}"#),
                                      sources: sources, home: home)
        #expect(bad.allSatisfy { $0.matches == 0 })
    }
}

/// What each harness covers.
@Suite struct SpacesCoverageTests {
    @Test func tableCountsEachHarness() {
        let rows = HarnessCoverage.table(sessions: [
            PinnedSession(harness: "claude", sessionID: "a", state: "active", toolEvents: 10, refusals: 1),
            PinnedSession(harness: "claude", sessionID: "b", state: "ended", toolEvents: 5),
            PinnedSession(harness: "claude", state: "unpinned"),
            PinnedSession(harness: "codex", sessionID: "c", state: "idle", toolEvents: 2),
            PinnedSession(harness: "gemini", state: "unpinned"),
            PinnedSession(harness: "other", state: "unpinned"),
        ])
        #expect(rows.map(\.harness) == ["claude", "codex", "gemini", "other"])
        let claude = rows[0]
        #expect(claude.pinnedSessions == 1 && claude.unpinnedProcesses == 1 && claude.toolEvents == 15 && claude.refusals == 1)
        #expect(claude.gap && claude.toolGuard && claude.directoryMoves)
        #expect(!rows[1].gap && !rows[1].directoryMoves)
        #expect(!rows[2].pins && !rows[2].toolGuard && rows[2].gap)
        #expect(rows[3].title == "other" && !rows[3].pins)
    }
}

/// SP-053.1.a2: no view model holds a term.
@Suite struct SpacesTermsTests {
    @Test @MainActor func noModelHoldsATerm() async throws {
        let api = FakeEditorAPI()
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(model.termsInfo("acme")?.summary == "2 terms · fingerprint 0a1b2c3d4e5f")
        #expect(await model.setTerms(space: "acme", text: fixtureTermsText))
        // The text reached orchd once, whole.
        #expect(api.sentTerms.map(\.text) == [fixtureTermsText])
        #expect(model.termsInfo("acme")?.count == 3)
        #expect(model.notice?.contains("3 terms") == true)
        // A refused send keeps nothing either.
        api.termsRefusal = RPCError(code: .invalidArgument, message: "the terms file of space \"acme\": line 2 is not a valid regular expression")
        #expect(await model.setTerms(space: "acme", text: fixtureTermsText) == false)
        #expect(model.notice?.contains("line 2") == true)
        // A first terms file for a space orchd had not described.
        api.termsRefusal = nil
        #expect(await model.setTerms(space: "globex", text: "Globexonly\n"))
        #expect(model.termsInfo("globex")?.count == 1)

        // Every stored property of the model, except the fake orchd it
        // talks to, which records what it was sent.
        var dumped = ""
        for child in Mirror(reflecting: model).children where child.label != "api" {
            dump(child.value, to: &dumped)
        }
        let labels = Set(Mirror(reflecting: model).children.compactMap(\.label))
        #expect(labels.isSuperset(of: ["_terms", "_notice", "_edit", "tester"]))
        dump(model.spaces, to: &dumped)
        dump(model.terms, to: &dumped)
        dumped += model.edit.json + (model.notice ?? "") + model.terms.map(\.summary).joined()
        for term in fixtureTerms + ["Globexonly"] {
            #expect(!dumped.contains(term), "the model holds \(term)")
        }
        #expect(dumped.contains("fingerprint"))
    }

    @Test func summaries() {
        #expect(SpaceTermsInfo(space: "a").summary == "No terms file")
        #expect(SpaceTermsInfo(space: "a", termsFile: "/t").summary == "No terms yet")
        #expect(SpaceTermsInfo(space: "a", termsFile: "/t", exists: true, count: 1, fingerprint: "ab").summary == "1 term · fingerprint ab")
        #expect(SpaceTermsInfo(space: "a", termsFile: "/t", exists: true, problem: "line 3").summary == "line 3")
    }
}

/// The editor model's load, save, revert, notices and retag.
@Suite struct SpacesEditorModelTests {
    @Test @MainActor func loadsTheNewestDraftAndSavesEdits() async throws {
        let api = FakeEditorAPI()
        api.diff = SpacesPolicyDiff(appliedRevision: 4, appliedJSON: editorFixturePolicy, draftRevision: 6,
                                    draftJSON: #"{"spaces":[{"name":"solo","kind":"private"}]}"#, draftByOperator: false)
        api.sources = SourceList(sources: [SourceSummary(uri: "repo://acme/app/README.md", chunks: 3)], totalSources: 1, totalChunks: 3)
        api.quarantine = [QuarantinedSource(uri: "file:///q1", sourceType: "file", chunks: 2)]
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(model.spaces.map(\.name) == ["solo"])
        #expect(!model.dirty && model.saveBlocker == "Nothing has changed.")
        #expect(model.sources.map(\.space) == ["quarantine", ""])
        #expect(model.count("acme")?.chunks == 120)
        #expect(model.retagTargets == ["acme", "globex", "notes", "oss", "personal", "quarantine"])
        #expect(model.coverage.count == 3 && model.contentKinds.count == 6 && model.hooksLayer == nil)

        model.addSpace(name: "acme", kind: "isolated")
        model.setValue("kind", of: 0, to: "isolated")
        model.setPublicRootSessions("solo")
        #expect(model.dirty && model.saveBlocker == nil)
        // A reload keeps unsaved edits.
        await model.load()
        #expect(model.spaces.count == 2)
        await model.save()
        #expect(api.saved.count == 1)
        #expect(model.notice?.contains("draft revision 5") == true)
        #expect(!model.dirty)

        model.addSpace(name: "", kind: "private")
        #expect(model.saveBlocker == "Every space needs a name.")
        model.revert()
        #expect(!model.dirty && model.notice == nil)
        model.removeSpace(at: 1)
        api.saveResult = .success(SavedSpacesDraft(revision: 7, problems: [SpacesProblemInfo(rule: "x", message: "bad")]))
        await model.save()
        #expect(model.savedProblems.count == 1 && model.notice?.contains("a problem") == true)
        model.addSpace(name: "z", kind: "private")
        api.saveResult = .failure(RPCError(code: .permissionDenied, message: "only the operator can save"))
        await model.save()
        #expect(model.notice == "only the operator can save" && model.dirty)
    }

    @Test @MainActor func offAndTurningOnSayWhy() async {
        let api = FakeEditorAPI()
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(model.offNotice == nil)
        api.state = "off"
        await model.load()
        #expect(model.offNotice?.contains("Settings → Spaces") == true)
        api.state = "turning_on"
        await model.load()
        #expect(model.offNotice?.contains("turning on") == true)
    }

    @Test @MainActor func failuresAreShown() async {
        let api = FakeEditorAPI()
        api.diffError = RPCError(code: .permissionDenied, message: "only the operator can read the draft")
        api.termsRefusal = RPCError(code: .unavailable, message: "spaces are unavailable")
        api.quarantineError = RPCError(code: .unavailable, message: "quarantine unavailable")
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        #expect(model.loadError == "only the operator can read the draft")
        #expect(model.termsError == "spaces are unavailable")
        #expect(model.sourcesError == "quarantine unavailable")
        #expect(model.retagTargets == ["quarantine"])
    }

    @Test @MainActor func retagConfirmsAndSendsTheSourcesSpace() async {
        let api = FakeEditorAPI()
        api.quarantine = [QuarantinedSource(uri: "file:///q1", sourceType: "file", chunks: 2)]
        api.sources = SourceList(sources: [SourceSummary(uri: "note://n", chunks: 1)], totalSources: 1, totalChunks: 1)
        let model = SpacesEditorModel(api: api, home: home)
        await model.load()
        let q = model.sources[0], n = model.sources[1]
        #expect(model.retagWarning(q, to: "oss").contains("readable from all of them"))
        #expect(model.retagWarning(q, to: "oss").contains("leaves quarantine"))
        #expect(model.retagWarning(n, to: "personal").contains("job's space"))
        await model.retag(q, to: "personal")
        #expect(api.retags.first?.expectFrom == "quarantine" && api.retags.first?.confirm == true)
        #expect(model.notice?.contains("Moved") == true)
        api.retagResult = .success(RetagOutcome(from: ["personal"], chunks: 1, moved: false))
        await model.retag(n, to: "personal")
        #expect(api.retags.last?.expectFrom == "" && model.notice?.contains("already in") == true)
        api.retagResult = .failure(RPCError(code: .failedPrecondition, message: "confirm required"))
        await model.retag(n, to: "oss")
        #expect(model.notice == "confirm required")
    }
}

/// SP-053.1.a3: the tester's answers equal ExplainPlacement's for the SP-022
/// cases, paths and every other kind of target, through the fake orchd.
@Suite struct PlacementTesterTests {
    private struct Case: Decodable {
        let path: String
        let explanation: JSONValue
    }

    private static let code = "/Users/fixture/code"

    private static func cases() throws -> [(target: String, answer: Orchd_V1_PlacementExplanation)] {
        let dir = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Fixtures")
        var out: [(String, Orchd_V1_PlacementExplanation)] = []
        for file in ["placement-explanations.json", "placement-tester.json"] {
            let cs = try JSONDecoder().decode([Case].self, from: Data(contentsOf: dir.appendingPathComponent(file)))
            for c in cs {
                let json = c.explanation.text.replacingOccurrences(of: "$CODE", with: code)
                out.append((c.path.replacingOccurrences(of: "$CODE", with: code), try Orchd_V1_PlacementExplanation(jsonString: json)))
            }
        }
        return out
    }

    @Test func testerMatchesExplainPlacementForEverySP022Case() async throws {
        let cases = try Self.cases()
        #expect(Set(cases.map(\.answer.targetKind)) == ["path", "url", "repo", "chat"])
        #expect(Set(cases.map(\.answer.outcome)) == ["placed", "quarantined", "conflict"])
        var service = FakeOrchdService()
        service.placements = Dictionary(uniqueKeysWithValues: cases.map { ($0.target, $0.answer) })
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let matched = await Self.check(cases, client)
            #expect(matched == cases.count)
        }
    }

    @MainActor
    private static func check(_ cases: [(target: String, answer: Orchd_V1_PlacementExplanation)], _ client: DaemonClient) async -> Int {
        let tester = PlacementTester(home: "/Users/fixture") { target in try await client.explainPlacement(target: target) }
        var matched = 0
        for c in cases {
            tester.input = c.target.replacingOccurrences(of: "/Users/fixture/", with: "~/")
            await tester.check()
            let want = c.answer
            guard let got = tester.explanation else {
                Issue.record("\(c.target): \(tester.error ?? "no answer")")
                continue
            }
            let same = tester.checked?.target == c.target && got.targetKind == want.targetKind
                && got.roots.map(\.root) == want.roots.map(\.root) && got.roots.map(\.space) == want.roots.map(\.space)
                && got.roots.map(\.kind) == want.roots.map(\.kind)
                && got.rule == want.rule && got.reason == want.reason && got.outcome == want.outcome && got.space == want.space
                && got.remote?.effect == (want.hasRemote ? want.remote.effect : nil)
                && got.remote?.space == (want.hasRemote ? want.remote.space : nil)
                && got.sessionSpace == want.sessionSpace && got.sessionReads == want.sessionReads
                && tester.chain.last?.decides ?? true
            #expect(same, "\(c.target)")
            if same { matched += 1 }
        }
        tester.input = "nonsense words"
        await tester.check()
        #expect(tester.error != nil && tester.explanation == nil)
        tester.input = "https://unknown.example/"
        await tester.check()
        #expect(tester.error == "no fixture for https://unknown.example/")
        return matched
    }

    @Test func parsesEveryKindOfTarget() {
        func parse(_ s: String) -> PlacementTarget? { try? PlacementTarget.parse(s, home: "/h").get() }
        #expect(parse("~") == .path("/h"))
        #expect(parse(" ~/x ") == .path("/h/x"))
        #expect(parse("/abs") == .path("/abs"))
        #expect(parse("HTTPS://a.dev/x") == .url("HTTPS://a.dev/x"))
        #expect(parse("acme/app") == .repo("acme/app"))
        #expect(parse("teams:T1") == .chat("teams:T1"))
        #expect(parse("") == nil && parse("a/b/c") == nil && parse("rel") == nil && parse("a /b") == nil)
    }
}

/// The editor's RPCs through the fake orchd.
@Suite struct SpacesEditorClientTests {
    @Test func daemonClientMapsTheEditorsCalls() async throws {
        var base = FakeOrchdService()
        var count = Orchd_V1_SpaceCount()
        count.space = "acme"
        count.chunks = 12
        count.stale = 2
        base.status.spaces.counts.spaces = [count]
        var editor = FakeEditorService()
        var t = Orchd_V1_SpaceTerms()
        t.space = "acme"
        t.termsFile = "~/t"
        t.exists = true
        t.count = 3
        t.fingerprint = "abc"
        editor.terms.spaces = [t]
        editor.setTerms = t
        var q = Orchd_V1_QuarantineSource()
        q.sourceUri = "file:///q1"
        q.sourceType = "file"
        q.chunks = 4
        editor.quarantine.sources = [q]
        editor.retag.from = ["quarantine"]
        editor.retag.chunks = 4
        editor.retag.moved = true
        try await withFakeOrchd(services: [base, editor]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let want = SpaceTermsInfo(space: "acme", termsFile: "~/t", exists: true, count: 3, fingerprint: "abc")
            #expect(try await client.spaceTerms() == [want])
            #expect(try await client.setSpaceTerms(space: "acme", terms: fixtureTermsText) == want)
            let sent = editor.recorder.last(Orchd_V1_SetSpaceTermsRequest.self)
            #expect(sent?.space == "acme" && sent?.terms == fixtureTermsText)
            #expect(try await client.quarantinedSources() == [QuarantinedSource(uri: "file:///q1", sourceType: "file", chunks: 4)])
            #expect(try await client.retagSource(uri: "file:///q1", space: "acme", confirm: true, expectFrom: "quarantine")
                == RetagOutcome(from: ["quarantine"], chunks: 4, moved: true))
            let r = editor.recorder.last(Orchd_V1_RetagSourceRequest.self)
            #expect(r?.sourceUri == "file:///q1" && r?.space == "acme" && r?.confirm == true && r?.expectFrom == "quarantine")
            #expect(try await client.spaceChunkCounts() == [SpaceChunkCount(space: "acme", chunks: 12, stale: 2)])
        }
    }
}
