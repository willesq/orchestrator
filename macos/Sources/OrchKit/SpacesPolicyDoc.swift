import Foundation

/// What one draft problem is about: a whole space, one value of a space's
/// list, one of a space's settings, or one of the policy's own fields.
enum PolicyTarget: Hashable, Sendable {
    case space(Int)
    case value(Int, String, Int)
    case field(Int, String)
    case policyField(String)
}

/// A draft policy read against the applied one: its review lines, which
/// line each problem orchd reported is about, and the draft with chosen
/// lines reverted to the applied revision.
///
/// orchd's problems carry a rule, the space and a message that quotes the
/// offending value (Go's %q), so the line is found from those; a problem
/// whose line cannot be found is shown on its own and still blocks Apply.
struct PolicyDoc {
    static let listFields = ["github_orgs", "roots", "public_paths", "web_prefixes", "chat", "reads"]
    static let fixedFields = ["kind", "terms_file", "gh_config_dir", "claude_config_dir"]

    let draft: JSONValue
    let applied: JSONValue
    let home: String
    let draftSpaces: [JSONValue]
    let appliedSpaces: [JSONValue]
    /// Every binding of the applied revision, per field, as `bindingKey`.
    let appliedBindings: [String: Set<String>]

    /// The fields that bind content to a space.
    static let bindingFields = ["github_orgs", "roots", "public_paths", "web_prefixes", "chat"]

    init(draft: JSONValue, applied: JSONValue, home: String) {
        self.draft = draft
        self.applied = applied
        self.home = home
        draftSpaces = draft["spaces"]?.arrayValue ?? []
        appliedSpaces = applied["spaces"]?.arrayValue ?? []
        var bound: [String: Set<String>] = [:]
        for s in appliedSpaces {
            for f in Self.bindingFields {
                for v in s[f]?.arrayValue ?? [] { bound[f, default: []].insert(Self.bindingKey(f, v, home: home)) }
            }
        }
        appliedBindings = bound
    }

    /// A binding as orchd compares it: roots expanded, case folded.
    static func bindingKey(_ field: String, _ v: JSONValue, home: String) -> String {
        let s = v.display
        let expanded = field == "roots" ? expand(s, home: home) : ""
        return (expanded.isEmpty ? s : expanded).lowercased()
    }

    /// Whether a draft binding is bound nowhere in the applied revision.
    /// One that is was moved from another space: dropping it would leave it
    /// unbound rather than back where the applied revision has it.
    func isFresh(_ field: String, _ v: JSONValue) -> Bool {
        guard Self.bindingFields.contains(field) else { return true }
        return !(appliedBindings[field]?.contains(Self.bindingKey(field, v, home: home)) ?? false)
    }

    /// Whether every binding of a draft space is fresh, so dropping the
    /// whole space unbinds nothing the applied revision binds.
    func allFresh(_ i: Int) -> Bool {
        Self.bindingFields.allSatisfy { f in (draftSpaces[i][f]?.arrayValue ?? []).allSatisfy { isFresh(f, $0) } }
    }

    static func name(_ space: JSONValue) -> String { space["name"]?.stringValue ?? "" }

    /// The applied space a draft space is compared with; nil for a new one.
    /// A second space of the same name is new: the applied revision cannot
    /// have two.
    func appliedSpace(_ i: Int) -> JSONValue? {
        let n = Self.name(draftSpaces[i])
        guard draftSpaces.firstIndex(where: { Self.name($0) == n }) == i else { return nil }
        return appliedSpaces.first { Self.name($0) == n }
    }

    func isNew(_ i: Int) -> Bool { appliedSpace(i) == nil }

    /// Per value of a draft space's list: true when the applied space does
    /// not have it (each applied value matches one draft value).
    func added(_ i: Int, _ field: String) -> [Bool] {
        var left = appliedSpace(i)?[field]?.arrayValue ?? []
        return (draftSpaces[i][field]?.arrayValue ?? []).map { v in
            if let k = left.firstIndex(of: v) {
                left.remove(at: k)
                return false
            }
            return true
        }
    }

    /// The applied values a draft space's list no longer has.
    func removed(_ i: Int, _ field: String) -> [JSONValue] {
        var left = appliedSpace(i)?[field]?.arrayValue ?? []
        for v in draftSpaces[i][field]?.arrayValue ?? [] {
            if let k = left.firstIndex(of: v) { left.remove(at: k) }
        }
        return left
    }

    /// Problems between two spaces: dropping either side would pick which
    /// space gets the org, folder, prefix, tenant or name, a choice that is
    /// the user's, so these are fixed in the draft.
    static let conflictRules: Set<String> = [
        "duplicate_name", "org_bound_twice", "prefix_bound_twice", "tenant_bound_twice", "root_bound_twice",
        "isolated_root_in_other_isolated",
    ]

    /// Whether the app may drop the target for these problems: it only
    /// undoes something the draft adds or changes, and no problem on it is
    /// a choice between two spaces.
    func droppable(_ target: PolicyTarget, _ problems: [SpacesProblemInfo]) -> Bool {
        excludable(target) && !problems.contains { Self.conflictRules.contains($0.rule) }
    }

    /// Whether dropping the target only undoes something the draft adds or
    /// changes.
    func excludable(_ target: PolicyTarget) -> Bool {
        switch target {
        case .space(let i):
            return isNew(i) && allFresh(i)
        case .value(let i, let f, let j):
            let flags = added(i, f)
            guard let values = draftSpaces[i][f]?.arrayValue, j < values.count, j < flags.count else { return false }
            return (isNew(i) || flags[j]) && isFresh(f, values[j])
        case .field(let i, let k):
            // Reverting a new space's setting drops the whole space.
            return isNew(i) ? allFresh(i) : draftSpaces[i][k] != appliedSpace(i)?[k]
        case .policyField(let k):
            return draft[k] != applied[k]
        }
    }

    // MARK: Problems to lines

    /// Each problem's target, or unplaced when its line cannot be found.
    func place(_ problems: [SpacesProblemInfo]) -> (byTarget: [PolicyTarget: [SpacesProblemInfo]], unplaced: [SpacesProblemInfo]) {
        var byTarget: [PolicyTarget: [SpacesProblemInfo]] = [:]
        var unplaced: [SpacesProblemInfo] = []
        var used: Set<PolicyTarget> = []
        for p in problems {
            if let t = target(p, used: used) {
                byTarget[t, default: []].append(p)
                used.insert(t)
            } else {
                unplaced.append(p)
            }
        }
        return (byTarget, unplaced)
    }

    private func target(_ p: SpacesProblemInfo, used: Set<PolicyTarget>) -> PolicyTarget? {
        let quoted = Self.goQuoted(p.message)
        let first = draftSpaces.firstIndex { Self.name($0) == p.space }
        switch p.rule {
        case "duplicate_name":
            return draftSpaces.lastIndex { Self.name($0) == p.space }.map(PolicyTarget.space)
        case "bad_name":
            return first.map(PolicyTarget.space)
        case "unknown_kind":
            return first.map { .field($0, "kind") }
        case "public_root_sessions":
            return .policyField("public_root_sessions")
        case "org_bound_twice", "prefix_bound_twice", "tenant_bound_twice", "root_bound_twice",
             "nested_root_less_strict", "isolated_root_in_other_isolated":
            let field = ["org_bound_twice": "github_orgs", "prefix_bound_twice": "web_prefixes",
                         "tenant_bound_twice": "chat"][p.rule] ?? "roots"
            return value(in: first, field, quoted.first, used: used)
        case "unknown_read", "public_reads", "isolated_reads_isolated", "private_reads_isolated":
            return value(in: first, "reads", quoted.dropFirst().first, used: used)
        case "terms_file_in_git_tree":
            return first.map { .field($0, "terms_file") }
        case "gh_config_dir_in_git_tree", "gh_config_dir_shared":
            return first.map { .field($0, "gh_config_dir") }
        case "claude_config_dir_in_git_tree", "claude_config_dir_shared":
            return first.map { .field($0, "claude_config_dir") }
        case "bad_binding":
            return badBinding(p.message, space: first, quoted: quoted, used: used)
        case "bad_public_path", "public_path_outside_root":
            // These name the entry by number, not by its pattern; a space
            // that may not list public_paths at all has no single line.
            return first.flatMap { publicPath($0, message: p.message) }
        default:
            return nil
        }
    }

    private func badBinding(_ message: String, space: Int?, quoted: [String], used: Set<PolicyTarget>) -> PolicyTarget? {
        // The phrase is read right after the space's quoted name, so a
        // value that contains a phrase cannot pick the field.
        let rest = Self.afterFirstQuoted(message)
        let lists = [(" has GitHub org ", "github_orgs"), (" has web prefix ", "web_prefixes"),
                     (" has chat tenant ", "chat"), (" has root ", "roots")]
        for (phrase, field) in lists where rest.hasPrefix(phrase) {
            return value(in: space, field, quoted.dropFirst().first, used: used)
        }
        let settings = [(" has terms file ", "terms_file"), (" has gh_config_dir ", "gh_config_dir"),
                        (" has claude_config_dir ", "claude_config_dir")]
        for (phrase, field) in settings where rest.hasPrefix(phrase) {
            return space.map { .field($0, field) }
        }
        return nil
    }

    /// The message after its first Go-quoted string; "" when it has none.
    static func afterFirstQuoted(_ message: String) -> Substring {
        guard let open = message.firstIndex(of: "\"") else { return "" }
        var i = message.index(after: open)
        while i < message.endIndex {
            let c = message[i]
            if c == "\\" {
                i = message.index(after: i)
                if i < message.endIndex { i = message.index(after: i) }
                continue
            }
            if c == "\"" { return message[message.index(after: i)...] }
            i = message.index(after: i)
        }
        return ""
    }

    /// The public_paths entry a message names as "entry <n>", counted from 1.
    private func publicPath(_ i: Int, message: String) -> PolicyTarget? {
        guard let r = message.range(of: "public_paths entry ") else { return nil }
        let n = Int(message[r.upperBound...].prefix { $0.isNumber }) ?? 0
        let count = draftSpaces[i]["public_paths"]?.arrayValue?.count ?? 0
        return n >= 1 && n <= count ? .value(i, "public_paths", n - 1) : nil
    }

    /// The last value of the space's list matching `quoted` not already
    /// claimed by another problem. Roots match as orchd expands them.
    private func value(in space: Int?, _ field: String, _ quoted: String?, used: Set<PolicyTarget>) -> PolicyTarget? {
        guard let i = space, let q = quoted else { return nil }
        let values = draftSpaces[i][field]?.arrayValue ?? []
        for j in values.indices.reversed() {
            guard let v = values[j].stringValue else { continue }
            let t = PolicyTarget.value(i, field, j)
            if !used.contains(t) && (v == q || (field == "roots" && Self.expand(v, home: home) == q)) { return t }
        }
        return nil
    }

    /// The strings a Go %q format put in `message`, in order.
    static func goQuoted(_ message: String) -> [String] {
        var out: [String] = []
        var it = message.unicodeScalars.makeIterator()
        while let c = it.next() {
            guard c == "\"" else { continue }
            var s = String.UnicodeScalarView()
            var closed = false
            while let d = it.next() {
                if d == "\"" {
                    closed = true
                    break
                }
                guard d == "\\", let e = it.next() else {
                    s.append(d)
                    continue
                }
                switch e {
                case "n": s.append("\n")
                case "t": s.append("\t")
                case "r": s.append("\r")
                case "x", "u", "U":
                    let width = e == "x" ? 2 : e == "u" ? 4 : 8
                    var hex = ""
                    for _ in 0..<width { if let h = it.next() { hex.unicodeScalars.append(h) } }
                    if let n = UInt32(hex, radix: 16), let scalar = Unicode.Scalar(n) { s.append(scalar) }
                default: s.append(e)
                }
            }
            if closed { out.append(String(s)) }
        }
        return out
    }

    /// A root as orchd's validator expands it: ~ is the home folder, and the
    /// path is cleaned lexically; "" when it is not absolute.
    static func expand(_ path: String, home: String) -> String {
        var p = path
        if p == "~" {
            p = home
        } else if p.hasPrefix("~/") {
            guard !home.isEmpty else { return "" }
            p = home + "/" + p.dropFirst(2)
        }
        guard p.hasPrefix("/") else { return "" }
        var parts: [Substring] = []
        for part in p.split(separator: "/") {
            switch part {
            case ".": continue
            case "..": if !parts.isEmpty { parts.removeLast() }
            default: parts.append(part)
            }
        }
        return "/" + parts.joined(separator: "/")
    }

    // MARK: Reverting

    /// The draft with each target reverted to the applied revision: an
    /// added value or space is removed, a changed setting gets its applied
    /// value back. Targets that are not excludable are left alone.
    func reverting(_ targets: Set<PolicyTarget>) -> JSONValue {
        var spaces = draftSpaces
        var dropSpaces: Set<Int> = []
        var dropValues: [String: [Int]] = [:]
        for t in targets where excludable(t) {
            switch t {
            case .space(let i):
                dropSpaces.insert(i)
            case .value(let i, let f, let j):
                dropValues["\(i)\u{0}\(f)", default: []].append(j)
            case .field(let i, let k):
                // A new space has nothing to go back to.
                if isNew(i) {
                    dropSpaces.insert(i)
                } else if case .object(var o) = spaces[i] {
                    o[k] = appliedSpace(i)?[k]
                    spaces[i] = .object(o)
                }
            case .policyField:
                break
            }
        }
        for (key, indices) in dropValues {
            let parts = key.split(separator: "\u{0}", maxSplits: 1)
            guard parts.count == 2, let i = Int(parts[0]), case .object(var o) = spaces[i],
                  var values = o[String(parts[1])]?.arrayValue else { continue }
            for j in Set(indices).sorted(by: >) where j < values.count { values.remove(at: j) }
            o[String(parts[1])] = .array(values)
            spaces[i] = .object(o)
        }
        for i in dropSpaces.sorted(by: >) { spaces.remove(at: i) }
        guard case .object(var top) = draft else { return draft }
        top["spaces"] = .array(spaces)
        for case .policyField(let k) in targets where excludable(.policyField(k)) {
            top[k] = applied[k]
        }
        return .object(top)
    }
}
