import Foundation

/// The editors of the Spaces pane's Edit page (SP-053).
public enum SpacesEditorTab: String, Sendable, CaseIterable, Identifiable {
    case spaces, access, rules, coverage, sources

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .spaces: "Spaces"
        case .access: "Access"
        case .rules: "Rules"
        case .coverage: "Coverage"
        case .sources: "Sources"
        }
    }
}

/// One source the Sources editor lists, from quarantine or the store.
public struct EditorSource: Sendable, Equatable, Identifiable {
    public var uri: String
    public var sourceType: String
    public var chunks: Int
    /// "quarantine" for a quarantined source; "" when orchd does not say.
    public var space: String

    public var id: String { uri }
}

/// Drives the advanced editors: the policy being edited (saved as a draft
/// that the Review page applies, D-71/D-78), the reads matrix, the roots
/// tree, the generated rules, the tester, harness coverage, the terms of
/// each space (counts and fingerprints only) and retagging sources.
///
/// Terms (INV-07): the model never holds a term. `setTerms` takes the text
/// the user typed, sends it, and keeps only orchd's count and fingerprint.
@MainActor
@Observable
public final class SpacesEditorModel {
    public var tab: SpacesEditorTab = .spaces
    /// The policy being edited, with every field the app does not know.
    public private(set) var edit = SpacesPolicyEdit(json: "")
    /// The document as loaded, to tell whether anything changed.
    public private(set) var loadedJSON = SpacesPolicyEdit(json: "").json
    public private(set) var diff: SpacesPolicyDiff?
    /// spaces.enabled: "on", "off" or "turning_on"; "" from an older orchd.
    public private(set) var spacesState = ""
    public private(set) var terms: [SpaceTermsInfo] = []
    public private(set) var termsError: String?
    /// The space whose terms are being sent.
    public private(set) var termsSending: String?
    public private(set) var sessions: [PinnedSession] = []
    public private(set) var enforcement: EnforcementState?
    public private(set) var sources: [EditorSource] = []
    public private(set) var sourcesError: String?
    public private(set) var counts: [SpaceChunkCount] = []
    public private(set) var loadError: String?
    public private(set) var isLoading = false
    public private(set) var isSaving = false
    /// What the last save, terms change or retag said.
    public private(set) var notice: String?
    /// orchd's problems with the last saved draft.
    public private(set) var savedProblems: [SpacesProblemInfo] = []
    public private(set) var accessError: String?
    /// The source being retagged.
    public private(set) var retagging: String?
    public let tester: PlacementTester

    private let api: any SpacesEditorAPI
    private let home: String
    /// Sources read for the rules' match counts and the Sources editor.
    public static let sourceLimit = 2000

    public init(api: any SpacesEditorAPI, home: String = NSHomeDirectory()) {
        self.api = api
        self.home = home
        tester = PlacementTester(home: home) { [api] target in try await api.explainPlacement(target: target) }
    }

    // MARK: Derived

    public var spaces: [EditableSpace] { edit.spaces }
    public var dirty: Bool { edit.json != loadedJSON }
    public var matrix: ReadsMatrix { ReadsMatrix(spaces: spaces) }
    public var rootsTree: RootsTree { RootsTree(spaces: spaces, home: home) }
    public var rules: [GeneratedRule] { GeneratedRule.rules(spaces: spaces, sources: sources.map(\.uri), home: home) }
    public var coverage: [HarnessCoverage] { HarnessCoverage.table(sessions: sessions) }
    public var contentKinds: [ContentKindRule] { ContentKindRule.table(publicRootSessions: edit.publicRootSessions) }
    public var hooksLayer: EnforcementSwitch? { enforcement?.switches.first { $0.layer == "hooks" } }

    /// Why the edits cannot be saved, or nil when they can.
    public var saveBlocker: String? {
        if !dirty { return "Nothing has changed." }
        let tree = rootsTree
        if tree.blocksSubmit {
            return tree.problemCount == 1
                ? "A root breaks the nesting rules; fix it in the roots tree first."
                : "\(tree.problemCount) roots break the nesting rules; fix them in the roots tree first."
        }
        let invalid = matrix.invalidCells.count
        if invalid > 0 {
            return invalid == 1 ? "A space reads a space it may not; switch that read off on Access."
                : "\(invalid) reads are not allowed; switch them off on Access."
        }
        if spaces.contains(where: { $0.name.isEmpty }) { return "Every space needs a name." }
        return nil
    }

    /// Why nothing here is enforced right now, or nil while spaces are on.
    public var offNotice: String? {
        switch spacesState {
        case "off":
            "Memory spaces are off, so orch keeps one shared memory and enforces nothing here. Edits are saved as drafts; they take effect once spaces are switched on in Settings → Spaces and the draft is applied."
        case "turning_on":
            "Memory spaces are turning on: orchd is splitting the store into the spaces' collections, and enforcement starts when it is done."
        default:
            nil
        }
    }

    /// The terms of a space, as orchd last described them.
    public func termsInfo(_ space: String) -> SpaceTermsInfo? { terms.first { $0.space == space } }

    /// The chunk counts of a space (RC-1101), when orchd counted them.
    public func count(_ space: String) -> SpaceChunkCount? { counts.first { $0.space == space } }

    /// Spaces a source can be retagged to: the applied ones and quarantine.
    public var retagTargets: [String] {
        let applied = (JSONValue.parse(diff?.appliedJSON ?? "")?["spaces"]?.arrayValue ?? []).compactMap { $0["name"]?.stringValue }
        return applied.sorted() + ["quarantine"]
    }

    // MARK: Loading

    public func load() async {
        isLoading = true
        defer { isLoading = false }
        if let status = try? await api.status() { spacesState = status.spacesState }
        do {
            let d = try await api.diffSpaces()
            diff = d
            let json = d.draftRevision != 0 ? d.draftJSON : d.appliedJSON
            // Reason: a reload must not throw away edits the user has not saved.
            if !dirty {
                edit = SpacesPolicyEdit(json: json)
                loadedJSON = edit.json
            }
            loadError = nil
        } catch {
            loadError = spacesRefusal(error)
        }
        do {
            terms = try await api.spaceTerms()
            termsError = nil
        } catch {
            termsError = spacesRefusal(error)
        }
        sessions = (try? await api.listSessions(includeEnded: false)) ?? sessions
        enforcement = (try? await api.enforcement()) ?? enforcement
        counts = (try? await api.spaceChunkCounts()) ?? counts
        await loadSources()
    }

    private func loadSources() async {
        var rows: [EditorSource] = []
        var problem: String?
        do {
            rows += try await api.quarantinedSources().map {
                EditorSource(uri: $0.uri, sourceType: $0.sourceType, chunks: $0.chunks, space: "quarantine")
            }
        } catch {
            problem = spacesRefusal(error)
        }
        do {
            let held = Set(rows.map(\.uri))
            rows += try await api.listSources(type: "", limit: Self.sourceLimit).sources.filter { !held.contains($0.uri) }.map {
                EditorSource(uri: $0.uri, sourceType: $0.sourceType, chunks: $0.chunks, space: "")
            }
        } catch {
            problem = problem ?? spacesRefusal(error)
        }
        sources = rows
        sourcesError = problem
    }

    // MARK: Editing

    public func setList(_ field: String, of index: Int, to values: [String]) {
        edit.setList(field, of: index, to: values)
    }

    public func setValue(_ field: String, of index: Int, to value: String) {
        edit.setValue(field, of: index, to: value)
    }

    public func setPublicRootSessions(_ space: String) { edit.publicRootSessions = space }

    public func addSpace(name: String, kind: String) { edit.addSpace(name: name, kind: kind) }

    public func removeSpace(at index: Int) { edit.removeSpace(at: index) }

    /// Switches one reads cell. A locked cell is refused, with why.
    @discardableResult
    public func setRead(reader: String, target: String, on: Bool) -> Bool {
        switch matrix.reads(after: reader, target: target, on: on) {
        case .failure(let e):
            accessError = e.message
            return false
        case .success(let reads):
            guard let i = spaces.first(where: { $0.name == reader })?.id else { return false }
            edit.setList("reads", of: i, to: reads)
            accessError = nil
            return true
        }
    }

    /// Drops every unsaved edit.
    public func revert() {
        edit = SpacesPolicyEdit(json: loadedJSON)
        notice = nil
    }

    /// Saves the edits as a new draft revision. Nothing is enforced until the
    /// Review page applies it.
    public func save() async {
        guard saveBlocker == nil, !isSaving else { return }
        isSaving = true
        defer { isSaving = false }
        do {
            let saved = try await api.saveSpacesDraft(policyJSON: edit.json)
            savedProblems = saved.problems
            loadedJSON = edit.json
            notice = saved.problems.isEmpty
                ? "Saved as draft revision \(saved.revision). Review and apply it on the Review page; nothing is enforced until then."
                : "Saved as draft revision \(saved.revision), but orchd found \(saved.problems.count == 1 ? "a problem" : "\(saved.problems.count) problems"): it cannot be applied until they are fixed."
            if let d = try? await api.diffSpaces() { diff = d }
        } catch {
            notice = spacesRefusal(error)
        }
    }

    // MARK: Terms

    /// Replaces a space's terms with `text`. The text is sent and dropped:
    /// only orchd's count and fingerprint are kept. Returns whether orchd
    /// took it, so the field can be cleared.
    public func setTerms(space: String, text: String) async -> Bool {
        guard termsSending == nil else { return false }
        termsSending = space
        defer { termsSending = nil }
        do {
            let info = try await api.setSpaceTerms(space: space, terms: text)
            if let i = terms.firstIndex(where: { $0.space == space }) { terms[i] = info } else { terms.append(info) }
            notice = "Terms of \(displaySafe(space)) replaced: \(info.summary)."
            return true
        } catch {
            notice = spacesRefusal(error)
            return false
        }
    }

    // MARK: Retag

    /// What moving `source` to `space` does, to confirm before it runs:
    /// orchd needs a confirmation to move into a public space, out of an
    /// isolated one or out of quarantine, and the editor confirms every move.
    public func retagWarning(_ source: EditorSource, to space: String) -> String {
        let kind = spaces.first { $0.name == space }?.kind ?? ""
        var lines = ["Move \(displaySafe(source.uri)) (\(source.chunks) chunks) to \(displaySafe(space))?"]
        if kind == "public" { lines.append("Every space reads a public space, so its content becomes readable from all of them.") }
        if source.space == "quarantine" { lines.append("It leaves quarantine, where nothing reads it.") }
        if source.space.isEmpty { lines.append("If it is in a job's space now, other spaces may read it after the move.") }
        lines.append("Later refreshes keep it there.")
        return lines.joined(separator: " ")
    }

    /// Moves a source's stored content to `space`, as confirmed.
    public func retag(_ source: EditorSource, to space: String) async {
        guard retagging == nil else { return }
        retagging = source.uri
        defer { retagging = nil }
        do {
            let r = try await api.retagSource(uri: source.uri, space: space, confirm: true, expectFrom: source.space)
            notice = r.moved
                ? "Moved \(displaySafe(source.uri)) to \(displaySafe(space)): \(r.chunks) chunks."
                : "\(displaySafe(source.uri)) was already in \(displaySafe(space)); it stays there on later refreshes."
            await loadSources()
        } catch {
            notice = spacesRefusal(error)
        }
    }
}
