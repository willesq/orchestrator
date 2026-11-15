import Foundation

/// Space names and folder paths for the setup planner.
extension SpacesSetupPlanner {
    /// A valid space name from a GitHub owner: 1-32 lowercase letters,
    /// digits, - or _, starting with a letter.
    static func spaceName(_ owner: String) -> String {
        var s = String(owner.lowercased().unicodeScalars.map { c -> Character in
            let ok = ("a"..."z").contains(c) || ("0"..."9").contains(c) || c == "-" || c == "_"
            return ok ? Character(c) : "-"
        })
        if let first = s.unicodeScalars.first, !("a"..."z").contains(first) { s = "org-" + s }
        if s.isEmpty { s = "job" }
        return String(s.prefix(32))
    }

    /// `name`, or name-2, name-3… when taken; records the one returned.
    static func unique(_ name: String, taken: inout Set<String>) -> String {
        var candidate = name
        var n = 2
        while taken.contains(candidate) {
            let suffix = "-\(n)"
            candidate = String(name.prefix(32 - suffix.count)) + suffix
            n += 1
        }
        taken.insert(candidate)
        return candidate
    }

    static func strictness(_ kind: String) -> Int { SetupKind(policyKind: kind)?.strictness ?? 0 }

    static func parent(_ path: String) -> String {
        let p = (path as NSString).deletingLastPathComponent
        return p.isEmpty ? "/" : p
    }

    /// The deepest folder holding every path; nil for none.
    static func commonAncestor(_ paths: [String]) -> String? {
        guard var common = paths.first?.split(separator: "/", omittingEmptySubsequences: true).map(String.init) else { return nil }
        for p in paths.dropFirst() {
            let parts = p.split(separator: "/", omittingEmptySubsequences: true).map(String.init)
            var n = 0
            while n < min(common.count, parts.count) && common[n].lowercased() == parts[n].lowercased() { n += 1 }
            common = Array(common.prefix(n))
        }
        return "/" + common.joined(separator: "/")
    }

    /// Whether `path` is `root` or inside it; macOS volumes ignore case.
    static func isUnder(_ path: String, _ root: String) -> Bool {
        let p = path.lowercased(), r = root.lowercased()
        return p == r || p.hasPrefix(r.hasSuffix("/") ? r : r + "/")
    }

    static func deepest(_ path: String, in roots: [SetupRoot]) -> SetupRoot? {
        roots.filter { isUnder(path, $0.path) }.max { $0.path.count < $1.path.count }
    }

    static func expand(_ path: String, home: String) -> String {
        if path == "~" { return home }
        if path.hasPrefix("~/") { return home + path.dropFirst(1) }
        return path
    }

    /// `path` with the home folder as ~, as the policy and the wizard show it.
    static func abbreviated(_ path: String, home: String = NSHomeDirectory()) -> String {
        guard !home.isEmpty, home != "/" else { return path }
        if path == home { return "~" }
        if path.hasPrefix(home + "/") { return "~" + path.dropFirst(home.count) }
        return path
    }
}
