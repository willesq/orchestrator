import Foundation

/// One bound folder root in the roots tree.
public struct RootNode: Sendable, Equatable, Identifiable {
    /// The space's position and the root's position in its list.
    public var id: String
    /// The root as the policy writes it.
    public var root: String
    /// Expanded as orchd expands it; "" when it is not a full path.
    public var path: String
    public var space: String
    public var kind: String
    /// How many other roots hold it.
    public var depth: Int
    /// Why orchd would refuse it; empty when it is fine.
    public var problems: [String]

    /// The space content directly under it goes to; "refused" when orchd
    /// would refuse the policy for it.
    public var effectiveSpace: String { problems.isEmpty ? space : "refused" }
}

/// Every root of the policy being edited, nested, with each nesting orchd
/// would refuse marked (D-15, INV-14; `checkRoots` in validate.go): a root
/// bound twice, a root less strict than one above it, and another space's
/// root inside a job's (isolated) root. A policy with a marked root cannot
/// be saved from the editor.
public struct RootsTree: Sendable, Equatable {
    public let nodes: [RootNode]

    public init(spaces: [EditableSpace], home: String) {
        struct Root {
            var id: String, root: String, path: String, key: String, space: String, kind: String
            var problems: [String] = []
        }
        var roots: [Root] = []
        var firstOwner: [String: String] = [:]
        for s in spaces {
            for (j, r) in s.roots.enumerated() {
                let path = PolicyDoc.expand(r, home: home)
                var root = Root(id: "\(s.id)/\(j)", root: r, path: path, key: path.lowercased(), space: s.name, kind: s.kind)
                if path.isEmpty {
                    root.problems.append("It is not a full path; start it with / or ~.")
                } else if let owner = firstOwner[root.key] {
                    root.problems.append("The folder is already bound to \(displaySafe(owner)).")
                } else {
                    firstOwner[root.key] = s.name
                }
                roots.append(root)
            }
        }
        // Reason: orchd compares each pair of distinct roots, so an inner
        // root is refused for every outer one it breaks the rule with.
        let unique = roots.filter { $0.problems.isEmpty }
        for i in roots.indices where roots[i].problems.isEmpty {
            let inner = roots[i]
            for outer in unique where Self.isUnder(inner.key, outer.key) {
                if Self.strictness(inner.kind) < Self.strictness(outer.kind) {
                    roots[i].problems.append("It is less strict than \(displaySafe(outer.space)) (\(displaySafe(outer.kind))) at \(displaySafe(outer.root)) above it.")
                } else if outer.kind == "isolated" && inner.space != outer.space {
                    roots[i].problems.append("It is inside job \(displaySafe(outer.space))'s folder \(displaySafe(outer.root)).")
                }
            }
        }
        nodes = roots.sorted { ($0.key, $0.id) < ($1.key, $1.id) }.map { r in
            let depth = unique.filter { $0.key != r.key && !r.key.isEmpty && Self.isUnder(r.key, $0.key) }.count
            return RootNode(id: r.id, root: r.root, path: r.path, space: r.space, kind: r.kind, depth: depth, problems: r.problems)
        }
    }

    /// Whether orchd would refuse a root, so the editor must not save.
    public var blocksSubmit: Bool { nodes.contains { !$0.problems.isEmpty } }

    public var problemCount: Int { nodes.reduce(0) { $0 + $1.problems.count } }

    /// Whether `inner` lies strictly inside `outer` (both case-folded).
    static func isUnder(_ inner: String, _ outer: String) -> Bool {
        let base = outer.hasSuffix("/") ? outer : outer + "/"
        return inner.hasPrefix(base) && inner != outer
    }

    /// Kinds from least to most strict; an unknown kind counts as public, so
    /// it is never trusted to be strict.
    static func strictness(_ kind: String) -> Int {
        switch kind {
        case "isolated": 2
        case "private": 1
        default: 0
        }
    }
}
