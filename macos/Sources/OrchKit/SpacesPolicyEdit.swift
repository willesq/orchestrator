import Foundation

/// One space of the policy being edited, as the editors show it. The terms
/// file is a path; its terms never enter the policy or this value.
public struct EditableSpace: Sendable, Equatable, Identifiable {
    /// The space's position in the policy's list.
    public var id: Int
    public var name: String
    /// public | private | isolated, or whatever the document says.
    public var kind: String
    public var githubOrgs: [String]
    public var roots: [String]
    public var publicPaths: [String]
    public var webPrefixes: [String]
    public var chat: [String]
    public var reads: [String]
    public var termsFile: String
    public var ghConfigDir: String
    public var claudeConfigDir: String

    public init(id: Int, name: String, kind: String, githubOrgs: [String] = [], roots: [String] = [],
                publicPaths: [String] = [], webPrefixes: [String] = [], chat: [String] = [], reads: [String] = [],
                termsFile: String = "", ghConfigDir: String = "", claudeConfigDir: String = "") {
        self.id = id
        self.name = name
        self.kind = kind
        self.githubOrgs = githubOrgs
        self.roots = roots
        self.publicPaths = publicPaths
        self.webPrefixes = webPrefixes
        self.chat = chat
        self.reads = reads
        self.termsFile = termsFile
        self.ghConfigDir = ghConfigDir
        self.claudeConfigDir = claudeConfigDir
    }
}

/// A spaces policy document being edited. It edits the document itself, so
/// every field this app does not know survives a save, as the Review page's
/// exclusion does.
public struct SpacesPolicyEdit: Sendable, Equatable {
    public static let listFields = ["github_orgs", "roots", "public_paths", "web_prefixes", "chat", "reads"]
    public static let valueFields = ["name", "kind", "terms_file", "gh_config_dir", "claude_config_dir"]
    public static let kinds = ["isolated", "private", "public"]

    public private(set) var doc: JSONValue

    /// `json` is a policy document; one that cannot be read starts empty.
    public init(json: String) {
        if case .object(var o)? = JSONValue.parse(json) {
            if o["spaces"]?.arrayValue == nil { o["spaces"] = .array([]) }
            doc = .object(o)
        } else {
            doc = .object(["spaces": .array([])])
        }
    }

    /// The document as orchd takes it (SetSpacesDraft).
    public var json: String { doc.text }

    public var spaces: [EditableSpace] {
        rawSpaces.enumerated().map { i, s in
            func list(_ f: String) -> [String] { (s[f]?.arrayValue ?? []).map(\.display) }
            func value(_ f: String) -> String { s[f]?.stringValue ?? "" }
            return EditableSpace(id: i, name: value("name"), kind: value("kind"), githubOrgs: list("github_orgs"),
                                 roots: list("roots"), publicPaths: list("public_paths"), webPrefixes: list("web_prefixes"),
                                 chat: list("chat"), reads: list("reads"), termsFile: value("terms_file"),
                                 ghConfigDir: value("gh_config_dir"), claudeConfigDir: value("claude_config_dir"))
        }
    }

    /// Where sessions under a public root write; "" lets orchd pick.
    public var publicRootSessions: String {
        get { doc["public_root_sessions"]?.stringValue ?? "" }
        set { setTop("public_root_sessions", newValue.trimmingCharacters(in: .whitespacesAndNewlines)) }
    }

    /// Sets one of a space's lists: entries are trimmed, blanks dropped and
    /// repeats kept once, in order. An empty list removes the field.
    public mutating func setList(_ field: String, of index: Int, to values: [String]) {
        guard Self.listFields.contains(field) else { return }
        var seen = Set<String>()
        let clean = values.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty && seen.insert($0).inserted }
        updateSpace(index) { $0[field] = clean.isEmpty ? nil : .array(clean.map(JSONValue.string)) }
    }

    /// Sets one of a space's single values; "" removes it. Renaming a space
    /// renames it in every space's reads and in public_root_sessions too.
    public mutating func setValue(_ field: String, of index: Int, to value: String) {
        guard Self.valueFields.contains(field), rawSpaces.indices.contains(index) else { return }
        let v = value.trimmingCharacters(in: .whitespacesAndNewlines)
        let old = rawSpaces[index]["name"]?.stringValue ?? ""
        updateSpace(index) { $0[field] = v.isEmpty && field != "name" && field != "kind" ? nil : .string(v) }
        if field == "name", !old.isEmpty, old != v {
            renameReferences(from: old, to: v)
        }
    }

    /// Adds a space with no bindings.
    public mutating func addSpace(name: String, kind: String) {
        var s = rawSpaces
        s.append(.object(["name": .string(name.trimmingCharacters(in: .whitespacesAndNewlines)), "kind": .string(kind)]))
        setTop("spaces", s)
    }

    /// Removes a space, and it from every other space's reads.
    public mutating func removeSpace(at index: Int) {
        guard rawSpaces.indices.contains(index) else { return }
        let name = rawSpaces[index]["name"]?.stringValue ?? ""
        var s = rawSpaces
        s.remove(at: index)
        setTop("spaces", s)
        if !name.isEmpty {
            renameReferences(from: name, to: nil)
        }
    }

    // MARK: Document plumbing

    private var rawSpaces: [JSONValue] { doc["spaces"]?.arrayValue ?? [] }

    private mutating func setTop(_ key: String, _ value: [JSONValue]) { setTop(key, JSONValue.array(value)) }

    private mutating func setTop(_ key: String, _ value: String) {
        setTop(key, value.isEmpty ? nil : JSONValue.string(value))
    }

    private mutating func setTop(_ key: String, _ value: JSONValue?) {
        guard case .object(var o) = doc else { return }
        o[key] = value
        doc = .object(o)
    }

    private mutating func updateSpace(_ index: Int, _ change: (inout [String: JSONValue]) -> Void) {
        var s = rawSpaces
        guard s.indices.contains(index), case .object(var o) = s[index] else { return }
        change(&o)
        s[index] = .object(o)
        setTop("spaces", s)
    }

    /// Points reads and public_root_sessions at `new`, or drops them for nil.
    private mutating func renameReferences(from old: String, to new: String?) {
        for i in rawSpaces.indices {
            let reads = (rawSpaces[i]["reads"]?.arrayValue ?? []).map(\.display)
            guard reads.contains(old) else { continue }
            setList("reads", of: i, to: reads.compactMap { $0 == old ? new : $0 })
        }
        if publicRootSessions == old { publicRootSessions = new ?? "" }
    }
}
