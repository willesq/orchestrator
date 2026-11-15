import Foundation

/// One rule the policy generates from a binding, with how many stored
/// sources it names (the Rules editor, SP-053).
public struct GeneratedRule: Sendable, Equatable, Identifiable {
    public enum Kind: String, Sendable, CaseIterable {
        case root, githubOrg, webPrefix, chat, publicPath

        /// What the rule matches, for people.
        public var title: String {
            switch self {
            case .root: "Folder root"
            case .githubOrg: "GitHub owner"
            case .webPrefix: "Web prefix"
            case .chat: "Chat tenant"
            case .publicPath: "Public path"
            }
        }

        /// How the rule decides, in a few words.
        public var effect: String {
            switch self {
            case .root: "Files under it, unless a deeper root or a stricter remote decides"
            case .githubOrg: "Checkouts and repo docs of the owner; stricter than a root wins"
            case .webPrefix: "Pages under it; the longest prefix decides"
            case .chat: "Messages from the tenant"
            case .publicPath: "Makes local content under a public root public"
            }
        }
    }

    public var kind: Kind
    public var value: String
    public var space: String
    public var spaceKind: String
    /// Stored sources the rule names; nil when sources do not say (public
    /// paths, which decide per file).
    public var matches: Int?

    public var id: String { "\(kind.rawValue)\u{1F}\(space)\u{1F}\(value)" }

    /// The rules of `spaces`, roots deepest first, then owners, prefixes,
    /// tenants and public paths, each counted against `sources` (their URIs).
    public static func rules(spaces: [EditableSpace], sources: [String], home: String) -> [GeneratedRule] {
        let uris = sources.map { $0.lowercased() }
        func count(_ prefixes: [String]) -> Int {
            let ps = prefixes.map { $0.lowercased() }
            return uris.filter { u in ps.contains { u.hasPrefix($0) } }.count
        }
        // The URI itself or anything below it, at a path boundary.
        func under(_ base: String) -> Int {
            let exact = base.lowercased()
            let prefix = exact.hasSuffix("/") ? exact : exact + "/"
            return uris.filter { $0 == exact || $0.hasPrefix(prefix) }.count
        }
        var out: [GeneratedRule] = []
        for s in spaces {
            for r in s.roots {
                let path = PolicyDoc.expand(r, home: home)
                out.append(GeneratedRule(kind: .root, value: r, space: s.name, spaceKind: s.kind,
                                         matches: path.isEmpty ? 0 : under("file://" + path)))
            }
        }
        out.sort { a, b in
            let da = a.value.split(separator: "/").count, db = b.value.split(separator: "/").count
            return da != db ? da > db : a.value < b.value
        }
        for s in spaces {
            for org in s.githubOrgs {
                out.append(GeneratedRule(kind: .githubOrg, value: org, space: s.name, spaceKind: s.kind,
                                         matches: count(["repo://\(org)/"])))
            }
        }
        for s in spaces {
            for p in s.webPrefixes {
                out.append(GeneratedRule(kind: .webPrefix, value: p, space: s.name, spaceKind: s.kind, matches: under(p)))
            }
        }
        for s in spaces {
            for t in s.chat {
                let parts = t.split(separator: ":", maxSplits: 1).map(String.init)
                let m = parts.count == 2 ? count(["\(parts[0])://\(parts[1])/"]) : 0
                out.append(GeneratedRule(kind: .chat, value: t, space: s.name, spaceKind: s.kind, matches: m))
            }
        }
        for s in spaces {
            for p in s.publicPaths {
                out.append(GeneratedRule(kind: .publicPath, value: p, space: s.name, spaceKind: s.kind, matches: nil))
            }
        }
        return out
    }
}

/// What the "where would this go?" tester asks about.
public enum PlacementTarget: Sendable, Equatable {
    case path(String)
    case url(String)
    case repo(String)
    case chat(String)

    /// The text orchd's ExplainPlacement takes.
    public var target: String {
        switch self {
        case .path(let s), .url(let s), .repo(let s), .chat(let s): s
        }
    }

    /// Reads a typed target: a full path (`~` expands), an http(s) URL,
    /// owner/repo, or a chat tenant as slack:<id> or teams:<id>.
    public static func parse(_ typed: String, home: String) -> Result<PlacementTarget, PlacementInputError> {
        let t = typed.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else {
            return .failure(PlacementInputError(message: "Type a folder, a web address, owner/repo, or slack:<id> or teams:<id>."))
        }
        let lower = t.lowercased()
        if lower.hasPrefix("http://") || lower.hasPrefix("https://") { return .success(.url(t)) }
        if lower.hasPrefix("slack:") || lower.hasPrefix("teams:") { return .success(.chat(t)) }
        if t.hasPrefix("/") || t == "~" || t.hasPrefix("~/") {
            // As the folder validator reads one: ~ is the home folder.
            if t == "~" { return .success(.path(home)) }
            return .success(.path(t.hasPrefix("~/") ? home + t.dropFirst(1) : t))
        }
        let parts = t.split(separator: "/", omittingEmptySubsequences: false)
        if parts.count == 2, parts.allSatisfy({ !$0.isEmpty && !$0.contains(" ") }) { return .success(.repo(t)) }
        return .failure(PlacementInputError(message: "Use a full path (starting with / or ~), a web address, owner/repo, or slack:<id> or teams:<id>."))
    }
}

/// The "where would this go?" tester: any target orchd can place, answered
/// by ExplainPlacement itself, so the tester shows exactly what orchd does.
@MainActor
@Observable
public final class PlacementTester {
    public var input = ""
    public private(set) var explanation: PlacementExplanation?
    public private(set) var checked: PlacementTarget?
    public private(set) var error: String?
    public private(set) var isChecking = false

    private let explain: @Sendable (String) async throws -> PlacementExplanation
    private let home: String
    private var generation = 0

    public init(home: String = NSHomeDirectory(), explain: @escaping @Sendable (String) async throws -> PlacementExplanation) {
        self.home = home
        self.explain = explain
    }

    /// The roots, shallowest first, each marked when it decides.
    public var chain: [(root: PlacementRoot, decides: Bool)] {
        guard let roots = explanation?.roots else { return [] }
        return roots.enumerated().map { ($0.element, $0.offset == roots.count - 1) }
    }

    public func check() async {
        switch PlacementTarget.parse(input, home: home) {
        case .failure(let e):
            generation += 1
            error = e.message
            explanation = nil
            checked = nil
        case .success(let target):
            await ask(target)
        }
    }

    private func ask(_ target: PlacementTarget) async {
        generation += 1
        let mine = generation
        isChecking = true
        defer { if mine == generation { isChecking = false } }
        do {
            let answer = try await explain(target.target)
            guard mine == generation else { return }
            explanation = answer
            checked = target
            error = nil
        } catch {
            guard mine == generation else { return }
            explanation = nil
            checked = nil
            self.error = spacesRefusal(error)
        }
    }
}
