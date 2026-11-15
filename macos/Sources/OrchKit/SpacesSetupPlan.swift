import Foundation

/// How a group of checkouts should behave, in the wizard's plain terms.
public enum SetupKind: String, Sendable, CaseIterable, Identifiable {
    case job, personal, open, leaveOut

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .job: "Job: keep separate"
        case .personal: "Personal"
        case .open: "Open"
        case .leaveOut: "Leave out"
        }
    }

    public var shortTitle: String {
        switch self {
        case .job: "Job"
        case .personal: "Personal"
        case .open: "Open"
        case .leaveOut: "Leave out"
        }
    }

    /// The policy's kind; nil for a group left out.
    public var policyKind: String? {
        switch self {
        case .job: "isolated"
        case .personal: "private"
        case .open: "public"
        case .leaveOut: nil
        }
    }

    public init?(policyKind: String) {
        switch policyKind {
        case "isolated": self = .job
        case "private": self = .personal
        case "public": self = .open
        default: return nil
        }
    }

    /// public < private < isolated, as orchd orders them; nil when left out.
    var strictness: Int? {
        switch self {
        case .open: 0
        case .personal: 1
        case .job: 2
        case .leaveOut: nil
        }
    }
}

/// A space of the applied policy, as much of it as setup needs. The
/// policy document itself is kept whole, so fields setup does not know
/// survive a rerun.
public struct AppliedSpace: Sendable, Equatable, Decodable {
    public var name: String
    public var kind: String
    public var githubOrgs: [String]
    public var roots: [String]
    public var ghConfigDir: String

    public init(name: String, kind: String, githubOrgs: [String] = [], roots: [String] = [], ghConfigDir: String = "") {
        self.name = name
        self.kind = kind
        self.githubOrgs = githubOrgs
        self.roots = roots
        self.ghConfigDir = ghConfigDir
    }

    enum CodingKeys: String, CodingKey {
        case name, kind, roots
        case githubOrgs = "github_orgs"
        case ghConfigDir = "gh_config_dir"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        kind = try c.decode(String.self, forKey: .kind)
        githubOrgs = try c.decodeIfPresent([String].self, forKey: .githubOrgs) ?? []
        roots = try c.decodeIfPresent([String].self, forKey: .roots) ?? []
        ghConfigDir = try c.decodeIfPresent(String.self, forKey: .ghConfigDir) ?? ""
    }
}

/// The applied policy: its document, and its spaces for lookups.
public struct AppliedPolicy: Sendable, Equatable {
    public var json: String
    public var spaces: [AppliedSpace]

    public init(json: String = "", spaces: [AppliedSpace] = []) {
        self.json = json
        self.spaces = spaces
    }

    private struct Doc: Decodable { var spaces: [AppliedSpace]? }

    /// Reads DiffSpaces' applied document; "" is no policy.
    public static func parse(_ json: String) throws -> AppliedPolicy {
        guard !json.isEmpty else { return AppliedPolicy() }
        let doc = try JSONDecoder().decode(Doc.self, from: Data(json.utf8))
        return AppliedPolicy(json: json, spaces: doc.spaces ?? [])
    }

    /// The space binding `owner`, matched as GitHub does, ignoring case.
    public func space(binding owner: String) -> AppliedSpace? {
        let key = owner.lowercased()
        return spaces.first { $0.githubOrgs.contains { $0.lowercased() == key } }
    }
}

/// The checkouts of one GitHub owner, and what the wizard proposes for them.
public struct SetupGroup: Sendable, Equatable, Identifiable {
    /// The owner, lowercased.
    public var id: String
    public var owner: String
    public var checkouts: [ScannedCheckout]
    public var publicRepos: Int
    public var privateRepos: Int
    public var unknownRepos: Int
    /// A gh login is a member of the org.
    public var member: Bool
    /// The owner is one of the gh logins: the user's own account.
    public var own: Bool
    /// The applied space already binding the owner; setup leaves it as is.
    public var boundSpace: String?
    public var proposed: SetupKind

    public var locked: Bool { boundSpace != nil }

    /// Why the wizard proposes what it does, for people.
    public var evidence: String {
        var parts: [String] = []
        let total = publicRepos + privateRepos + unknownRepos
        if privateRepos == total { parts.append(total == 1 ? "private repo" : "all repos private") }
        else if publicRepos == total { parts.append(total == 1 ? "public repo" : "all repos public") }
        else {
            var v: [String] = []
            if publicRepos > 0 { v.append("\(publicRepos) public") }
            if privateRepos > 0 { v.append("\(privateRepos) private") }
            if unknownRepos > 0 { v.append("\(unknownRepos) not known") }
            parts.append(v.joined(separator: ", "))
        }
        if own { parts.append("your own account") } else if member { parts.append("you're a member") }
        return parts.joined(separator: " · ")
    }

    /// The checkouts GitHub does not report public: an open space never
    /// takes them, so they go to quarantine.
    public var notPublic: [ScannedCheckout] { checkouts.filter { $0.visibility != "public" } }
}

/// A folder root the wizard shows: bound already, proposed, or not bound
/// and why.
public struct SetupRoot: Sendable, Equatable, Identifiable {
    public var id: String { path.lowercased() + "|" + space }
    /// Absolute.
    public var path: String
    public var space: String
    /// The policy kind: public, private or isolated.
    public var kind: String
    /// In the applied policy already.
    public var existing: Bool
    /// Why it is not proposed; nil when it is bound.
    public var rejected: String?
    /// How many bound roots hold it, for the tree.
    public var depth: Int
    /// Checkouts it would decide.
    public var checkouts: Int
}

/// A space the wizard creates, or one it adds bindings to.
public struct SetupSpace: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public var name: String
    public var kind: SetupKind
    public var isNew: Bool
    public var orgs: [String]
    /// Absolute roots added.
    public var roots: [String]
    /// The job's gh profile (GH_CONFIG_DIR); "" for a space that is not a
    /// new job.
    public var ghConfigDir: String
}

/// Where a checkout would go under the proposed policy.
public struct SetupPlacement: Sendable, Equatable {
    public var checkout: ScannedCheckout
    /// The space, or "quarantine".
    public var space: String
    /// placed | quarantined | conflict
    public var outcome: String
    public var rule: String
}

/// Everything the wizard would change, and what that would do.
public struct SetupPlan: Sendable, Equatable {
    public var spaces: [SetupSpace]
    public var roots: [SetupRoot]
    public var placements: [SetupPlacement]
    /// The policy to save as a draft.
    public var policyJSON: String
    /// The policy differs from the applied one.
    public var changesPolicy: Bool

    /// The applied document could not be read, so no policy was built.
    public var unreadableBase: Bool { changesPolicy && policyJSON.isEmpty }

    public var newSpaces: [SetupSpace] { spaces.filter(\.isNew) }
    public var quarantined: [SetupPlacement] { placements.filter { $0.outcome == "quarantined" } }
    public var conflicts: [SetupPlacement] { placements.filter { $0.outcome == "conflict" } }

    /// Checkouts placed per space, by space name.
    public var placedCounts: [(space: String, count: Int)] {
        let placed = placements.filter { $0.outcome == "placed" }
        return Dictionary(grouping: placed, by: \.space).map { ($0.key, $0.value.count) }.sorted { $0.space < $1.space }
    }
}

/// Builds the wizard's proposals from a scan and the applied policy. Pure,
/// so every proposal is tested without orchd.
public enum SpacesSetupPlanner {
    /// The default gh profile folder for a job space.
    public static func defaultProfile(for space: String) -> String { "~/.config/gh-\(space)" }

    /// One group per GitHub owner with at least one believed remote.
    public static func groups(scan: SpacesScan, applied: AppliedPolicy) -> [SetupGroup] {
        let members = Set(scan.accounts.flatMap(\.orgs).map { $0.lowercased() })
        let logins = Set(scan.accounts.map { $0.login.lowercased() }.filter { !$0.isEmpty })
        var order: [String] = []
        var byOwner: [String: [ScannedCheckout]] = [:]
        for c in scan.checkouts where c.hasRemote {
            let key = c.owner.lowercased()
            if byOwner[key] == nil { order.append(key) }
            byOwner[key, default: []].append(c)
        }
        return order.sorted().map { key in
            let checkouts = byOwner[key] ?? []
            let pub = checkouts.filter { $0.visibility == "public" }.count
            let priv = checkouts.filter { $0.visibility == "private" }.count
            var g = SetupGroup(id: key, owner: checkouts.first?.owner ?? key, checkouts: checkouts, publicRepos: pub,
                               privateRepos: priv, unknownRepos: checkouts.count - pub - priv, member: members.contains(key),
                               own: logins.contains(key), boundSpace: applied.space(binding: key)?.name, proposed: .job)
            g.proposed = propose(g, applied: applied)
            return g
        }
    }

    /// The kind proposed for a group. A bound group keeps its space's kind;
    /// otherwise the user's own account is personal, an owner with mostly
    /// public repos is open, and anything else is a job, the strictest.
    static func propose(_ g: SetupGroup, applied: AppliedPolicy) -> SetupKind {
        if let bound = applied.space(binding: g.owner) { return SetupKind(policyKind: bound.kind) ?? .leaveOut }
        if g.own { return .personal }
        // Reason: when unsure, the stricter kind is the safe proposal; an
        // open owner's few private repos still go to quarantine, never public.
        if g.publicRepos > g.privateRepos + g.unknownRepos { return .open }
        return .job
    }

    /// The plan for the kinds chosen per group id and the gh profile typed
    /// per group id. `home` lets roots be written as ~ paths.
    public static func plan(groups: [SetupGroup], kinds: [String: SetupKind], profiles: [String: String],
                            scan: SpacesScan, applied: AppliedPolicy, home: String) -> SetupPlan {
        let kindOf: (SetupGroup) -> SetupKind = { g in g.locked ? g.proposed : (kinds[g.id] ?? g.proposed) }
        var names = Set(applied.spaces.map(\.name)).union(["quarantine", "shared"])
        var spaces: [SetupSpace] = []
        var target: [String: String] = [:] // group id -> space name

        // A personal or open group joins the applied space of that kind, if
        // there is one, else one new space shared by every such group.
        func shared(_ kind: SetupKind, preferred: String) -> String {
            if let s = spaces.first(where: { $0.kind == kind }) { return s.name }
            let existing = applied.spaces.filter { $0.kind == kind.policyKind }
            if let s = existing.first(where: { $0.name == preferred }) ?? existing.first {
                spaces.append(SetupSpace(name: s.name, kind: kind, isNew: false, orgs: [], roots: [], ghConfigDir: ""))
                return s.name
            }
            let name = unique(preferred, taken: &names)
            spaces.append(SetupSpace(name: name, kind: kind, isNew: true, orgs: [], roots: [], ghConfigDir: ""))
            return name
        }
        for g in groups {
            if let bound = g.boundSpace { target[g.id] = bound; continue }
            switch kindOf(g) {
            case .leaveOut: continue
            case .personal: target[g.id] = shared(.personal, preferred: "personal")
            case .open: target[g.id] = shared(.open, preferred: "public")
            case .job:
                let name = unique(spaceName(g.owner), taken: &names)
                let typed = profiles[g.id]?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                spaces.append(SetupSpace(name: name, kind: .job, isNew: true, orgs: [], roots: [],
                                         ghConfigDir: typed.isEmpty ? defaultProfile(for: name) : typed))
                target[g.id] = name
            }
            if let name = target[g.id], let i = spaces.firstIndex(where: { $0.name == name }) {
                spaces[i].orgs.append(g.owner)
            }
        }

        // Roots: one per job or personal group whose checkouts share a folder
        // that holds nothing it would wrongly claim. Open spaces get none
        // (D-65): local folders become public only through an explicit
        // public_paths list (SP-076), never by a root.
        let groupOf = Dictionary(uniqueKeysWithValues: groups.map { ($0.id, $0) })
        var candidates: [SetupRoot] = []
        for g in groups where !g.locked {
            let kind = kindOf(g)
            guard kind == .job || kind == .personal, let space = target[g.id] else { continue }
            let folders = g.checkouts.map { parent($0.path) }
            guard let folder = commonAncestor(folders) else { continue }
            let reason = rootProblem(folder, for: g, kind: kind, space: space, scan: scan, home: home, groups: groupOf,
                                     kindOf: kindOf, target: target)
            candidates.append(SetupRoot(path: folder, space: space, kind: kind.policyKind ?? "", existing: false,
                                        rejected: reason, depth: 0, checkouts: 0))
        }
        let existing = applied.spaces.flatMap { s in
            s.roots.map { SetupRoot(path: expand($0, home: home), space: s.name, kind: s.kind, existing: true, rejected: nil,
                                    depth: 0, checkouts: 0) }
        }
        var roots = nest(existing: existing, proposed: candidates)
        for r in roots where !r.existing && r.rejected == nil {
            if let i = spaces.firstIndex(where: { $0.name == r.space }) { spaces[i].roots.append(r.path) }
        }

        // Where each checkout would go, as orchd's classifier decides it.
        var orgSpace: [String: (String, String)] = [:]
        for s in applied.spaces { for o in s.githubOrgs { orgSpace[o.lowercased()] = (s.name, s.kind) } }
        for s in spaces { for o in s.orgs { orgSpace[o.lowercased()] = (s.name, s.kind.policyKind ?? "") } }
        let bound = roots.filter { $0.rejected == nil }
        let placements = scan.checkouts.map { place($0, roots: bound, orgs: orgSpace) }
        for i in roots.indices where roots[i].rejected == nil {
            let key = roots[i].path.lowercased()
            roots[i].checkouts = placements.filter { p in
                deepest(p.checkout.path, in: bound).map { $0.path.lowercased() == key } ?? false
            }.count
        }

        let spacesWithChanges = spaces.filter { $0.isNew || !$0.orgs.isEmpty || !$0.roots.isEmpty }
        // Reason: an applied document setup cannot read is never replaced by
        // a smaller one; the plan carries no policy and Review stops.
        let json = policyJSON(applied: applied.json, spaces: spacesWithChanges, home: home) ?? ""
        return SetupPlan(spaces: spacesWithChanges, roots: roots, placements: placements, policyJSON: json,
                         changesPolicy: !spacesWithChanges.isEmpty)
    }

    /// Why `folder` cannot be `g`'s root, or nil when it can. A root decides
    /// every checkout inside it unless a stricter remote wins, so it must not
    /// pull another job's checkouts in (a conflict), nor take an open owner's
    /// private repo out of quarantine.
    static func rootProblem(_ folder: String, for g: SetupGroup, kind: SetupKind, space: String, scan: SpacesScan,
                            home: String, groups: [String: SetupGroup], kindOf: (SetupGroup) -> SetupKind,
                            target: [String: String]) -> String? {
        let h = home.lowercased(), f = folder.lowercased()
        if folder == "/" || f == h || h.hasPrefix(f + "/") || (!h.isEmpty && !f.hasPrefix(h + "/")) {
            return "its checkouts share no folder below your home folder; its GitHub org places them"
        }
        if scan.truncated {
            return "the scan stopped early, so setup cannot tell what else the folder holds"
        }
        let mine = kind.strictness ?? 0
        for c in scan.checkouts where c.hasRemote && c.owner.lowercased() != g.id && isUnder(c.path, folder) {
            guard let other = groups[c.owner.lowercased()] else { continue }
            let k = kindOf(other)
            let theirs = k.strictness
            if theirs == mine && target[other.id] != space {
                return "it also holds \(displaySafe(other.owner))'s checkouts, which belong to another space"
            }
            if k == .open && c.visibility != "public" {
                return "it holds \(displaySafe(c.owner))/\(displaySafe(c.repo)), a private repo of an open org that should stay in quarantine"
            }
            // Reason: a job's root would pull the user's own or left-out
            // repos into the employer's space; only public forks may sit
            // inside one (they are public anyway).
            if kind == .job && k != .open && target[other.id] != space {
                return "it also holds \(displaySafe(other.owner))'s checkouts, which would join this job"
            }
        }
        return nil
    }

    /// Marks each proposed root the nesting rules refuse: a root may only be
    /// at least as strict as every root above it, and nothing of another
    /// space sits inside a job's root (INV-14). Shallow roots are decided
    /// first, so an inner root gives way to an outer one.
    static func nest(existing: [SetupRoot], proposed: [SetupRoot]) -> [SetupRoot] {
        var accepted = existing
        var out = existing
        for r in proposed.sorted(by: { $0.path.split(separator: "/").count < $1.path.split(separator: "/").count }) {
            var r = r
            if r.rejected == nil { r.rejected = nestProblem(r, among: accepted) }
            if r.rejected == nil { accepted.append(r) }
            out.append(r)
        }
        out.sort { $0.path.lowercased() < $1.path.lowercased() }
        for i in out.indices {
            out[i].depth = accepted.filter { a in a.id != out[i].id && isUnder(out[i].path, a.path) && a.path.lowercased() != out[i].path.lowercased() }.count
        }
        return out
    }

    static func nestProblem(_ r: SetupRoot, among accepted: [SetupRoot]) -> String? {
        let s = strictness(r.kind)
        for a in accepted {
            let as_ = strictness(a.kind)
            if a.path.lowercased() == r.path.lowercased() {
                return "the folder is already bound to \(displaySafe(a.space))"
            }
            if isUnder(r.path, a.path) {
                if s < as_ { return "it would be less strict than \(displaySafe(a.space)) (\(a.kind)) above it" }
                if a.kind == "isolated" && a.space != r.space { return "it is inside job \(displaySafe(a.space))'s folder" }
            } else if isUnder(a.path, r.path) {
                if as_ < s { return "\(displaySafe(a.space)) (\(a.kind)) inside it would be less strict" }
                if r.kind == "isolated" && a.space != r.space { return "it would hold \(displaySafe(a.space))'s folder" }
            }
        }
        return nil
    }

    /// Mirrors orchd's placeCheckout and publicProof (internal/spaces/classify.go)
    /// for an estimate; orchd decides for real once the policy is applied.
    static func place(_ c: ScannedCheckout, roots: [SetupRoot], orgs: [String: (String, String)]) -> SetupPlacement {
        let root = deepest(c.path, in: roots)
        let remote = c.hasRemote ? orgs[c.owner.lowercased()] : nil
        let veto = c.hasRemote && c.visibility != "public"
        func out(_ space: String, _ outcome: String, _ rule: String) -> SetupPlacement {
            // Local content under a public root is never proof it is public (D-65).
            if outcome != "conflict", let root, root.kind == "public", outcome == "placed" || rule == "not_verified_public" {
                return SetupPlacement(checkout: c, space: "quarantine", outcome: "quarantined", rule: "no_public_proof")
            }
            return SetupPlacement(checkout: c, space: space, outcome: outcome, rule: rule)
        }
        var chosen: (space: String, kind: String)
        var rule: String
        switch (root, remote) {
        case (nil, nil): return out("quarantine", "quarantined", "unmatched")
        case (let r?, nil): (chosen, rule) = ((r.space, r.kind), "root")
        case (nil, let m?): (chosen, rule) = ((m.0, m.1), "remote")
        case (let r?, let m?):
            if r.space == m.0 { (chosen, rule) = ((r.space, r.kind), "root_and_remote_agree") }
            else if strictness(m.1) > strictness(r.kind) { (chosen, rule) = ((m.0, m.1), "remote_stricter") }
            else if strictness(r.kind) > strictness(m.1) { (chosen, rule) = ((r.space, r.kind), "root_stricter") }
            else { return out("quarantine", "conflict", "conflict") }
        }
        if chosen.kind == "public" && veto { return out("quarantine", "quarantined", "not_verified_public") }
        return out(chosen.space, "placed", rule)
    }

    /// The applied document with the wizard's additions. Fields setup does
    /// not know are kept as they are.
    static func policyJSON(applied: String, spaces: [SetupSpace], home: String) -> String? {
        var doc: [String: Any] = ["spaces": [Any]()]
        if !applied.isEmpty {
            guard let read = (try? JSONSerialization.jsonObject(with: Data(applied.utf8))) as? [String: Any] else { return nil }
            doc = read
        }
        guard let list0 = doc["spaces"] as? [[String: Any]] ?? (doc["spaces"] == nil ? [] : nil) else { return nil }
        var list = list0
        for s in spaces {
            var i = list.firstIndex { ($0["name"] as? String) == s.name }
            if i == nil {
                list.append(["name": s.name, "kind": s.kind.policyKind ?? ""])
                i = list.count - 1
            }
            guard let i else { continue }
            func add(_ key: String, _ values: [String]) {
                guard !values.isEmpty else { return }
                var current = list[i][key] as? [String] ?? []
                for v in values where !current.contains(where: { $0.lowercased() == v.lowercased() }) { current.append(v) }
                list[i][key] = current
            }
            add("github_orgs", s.orgs)
            add("roots", s.roots.map { abbreviated($0, home: home) })
            if s.isNew && s.kind == .job && !s.ghConfigDir.isEmpty { list[i]["gh_config_dir"] = s.ghConfigDir }
        }
        doc["spaces"] = list
        guard let data = try? JSONSerialization.data(withJSONObject: doc, options: [.sortedKeys, .withoutEscapingSlashes]),
              let text = String(data: data, encoding: .utf8) else { return nil }
        return text
    }
}
