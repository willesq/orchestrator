import Foundation

/// One cell of the reads matrix: may sessions of `reader` read `target`?
public struct ReadsCell: Sendable, Equatable, Identifiable {
    public enum State: Sendable, Equatable {
        /// A space always reads itself.
        case itself
        /// Every space reads a public space; nothing to switch.
        case always
        /// orchd refuses this read (validate.go): a public space reads only
        /// public ones, and nothing but itself reads an isolated space.
        case locked(String)
        /// Allowed; on when the reader lists the target. `warning` says what
        /// turning it on shares.
        case switchable(warning: String?)
    }

    public var reader: String
    public var target: String
    public var state: State
    /// The reader lists the target in its reads.
    public var listed: Bool

    public var id: String { reader + "\u{1F}" + target }

    /// Whether sessions of the reader read the target under this policy.
    public var reads: Bool {
        switch state {
        case .itself, .always: true
        case .locked: false
        case .switchable: listed
        }
    }

    /// A locked cell the document lists anyway: orchd will refuse the draft
    /// until it is switched off.
    public var invalid: Bool {
        if case .locked = state { return listed }
        return false
    }

    /// Whether the UI may switch it on.
    public var canEnable: Bool {
        if case .switchable = state { return !listed }
        return false
    }

    /// Whether the UI may switch it off: a listed switchable cell, or a
    /// locked one the document lists by mistake.
    public var canDisable: Bool {
        switch state {
        case .switchable, .locked: listed
        case .itself, .always: false
        }
    }
}

/// Why the matrix refused a change.
public struct ReadsChangeError: Error, Equatable, Sendable {
    public let message: String
}

/// Which spaces read which (the Access editor, SP-053). The cells follow
/// orchd's rules: a space reads itself and every public space; a public
/// space reads nothing else; nothing reads an isolated space but itself;
/// a private space may read other private spaces, and an isolated (job)
/// space may read a private one, which shares personal content with the
/// job's sessions, so it carries a warning.
public struct ReadsMatrix: Sendable, Equatable {
    public let spaces: [EditableSpace]

    public init(spaces: [EditableSpace]) {
        self.spaces = spaces
    }

    public var names: [String] { spaces.map(\.name) }

    /// Every cell, by reader then target.
    public var rows: [[ReadsCell]] {
        spaces.map { r in spaces.map { t in cell(r, t) } }
    }

    public func cell(reader: String, target: String) -> ReadsCell? {
        guard let r = spaces.first(where: { $0.name == reader }), let t = spaces.first(where: { $0.name == target }) else {
            return nil
        }
        return cell(r, t)
    }

    /// Cells the document lists although orchd refuses them.
    public var invalidCells: [ReadsCell] { rows.flatMap { $0 }.filter(\.invalid) }

    /// The reader's reads after switching one cell. A locked cell can only
    /// be switched off, and only when the document lists it.
    public func reads(after reader: String, target: String, on: Bool) -> Result<[String], ReadsChangeError> {
        guard let r = spaces.first(where: { $0.name == reader }), let c = cell(reader: reader, target: target) else {
            return .failure(ReadsChangeError(message: "No such space."))
        }
        if on {
            guard c.canEnable else {
                switch c.state {
                case .locked(let why): return .failure(ReadsChangeError(message: why))
                case .itself, .always: return .failure(ReadsChangeError(message: "It is always read; there is nothing to switch."))
                case .switchable: return .success(r.reads)
                }
            }
            return .success(r.reads + [target])
        }
        guard c.canDisable else {
            if case .switchable = c.state { return .success(r.reads) }
            return .failure(ReadsChangeError(message: "It is always read; there is nothing to switch."))
        }
        return .success(r.reads.filter { $0 != target })
    }

    private func cell(_ r: EditableSpace, _ t: EditableSpace) -> ReadsCell {
        let listed = r.reads.contains(t.name)
        let state: ReadsCell.State
        if r.id == t.id || r.name == t.name {
            state = .itself
        } else if t.kind == "public" {
            state = .always
        } else if r.kind == "public" {
            state = .locked("A public space reads only public spaces, so its sessions can share what they read.")
        } else if t.kind == "isolated" {
            state = .locked(r.kind == "isolated"
                ? "One job never reads another; merge the two spaces instead."
                : "Nothing reads a job's space but the job itself.")
        } else if r.kind == "isolated" && t.kind == "private" {
            state = .switchable(warning: "Sessions in \(displaySafe(r.name)) would read your personal space \(displaySafe(t.name)), and could copy it into the job's work.")
        } else if (r.kind == "private" || r.kind == "isolated") && t.kind == "private" {
            state = .switchable(warning: nil)
        } else {
            // An unknown kind: orchd refuses the draft for it anyway.
            state = .locked("The space's kind is not public, private or isolated.")
        }
        return ReadsCell(reader: r.name, target: t.name, state: state, listed: listed)
    }
}

/// One row of the per-content-kind table: where a kind of content goes and
/// who reads it. The policy has no per-kind rules (they would come with the
/// chat connectors, SP-060), so this is what orchd's classifier does today.
public struct ContentKindRule: Sendable, Equatable, Identifiable {
    public var kind: String
    public var goesTo: String
    public var readBy: String
    public var id: String { kind }

    /// The table for a policy; `publicRootSessions` is where sessions under
    /// a public root write ("" for orchd's choice).
    public static func table(publicRootSessions: String) -> [ContentKindRule] {
        let sessions = publicRootSessions.isEmpty
            ? "the only private space, if there is exactly one; otherwise quarantine"
            : displaySafe(publicRootSessions)
        let readers = "The space itself and every space that lists it in its reads; every space when it is public."
        return [
            ContentKindRule(kind: "Local files and checkouts",
                            goesTo: "The deepest root holding it; a stricter git remote wins, and two jobs claiming it is a conflict. Under a public root only public_paths are public; the rest is quarantined.",
                            readBy: readers),
            ContentKindRule(kind: "GitHub repo docs",
                            goesTo: "The space of the repo's owner. A repo goes public only when GitHub reports it public; an unbound owner's goes to quarantine.",
                            readBy: readers),
            ContentKindRule(kind: "Web pages",
                            goesTo: "The space of the longest matching web prefix. A page a session stored goes to the stricter of that and the session's space (two spaces equally strict are a conflict), or the session's space with no prefix; with neither, quarantine.",
                            readBy: readers),
            ContentKindRule(kind: "Session transcripts and summaries",
                            goesTo: "The space its first folder is placed in; a session under a public root writes to \(sessions).",
                            readBy: readers),
            ContentKindRule(kind: "Notes from agents and the CLI",
                            goesTo: "The pinned space of the session that wrote them; with no pinned session, quarantine.",
                            readBy: readers),
            ContentKindRule(kind: "Chat",
                            goesTo: "The space the chat tenant is bound to. An unbound tenant, or one bound to a public space, goes to quarantine: chat is never verified public.",
                            readBy: readers),
        ]
    }
}
