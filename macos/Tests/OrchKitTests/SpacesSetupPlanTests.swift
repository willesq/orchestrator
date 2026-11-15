import Foundation
import Testing
@testable import OrchKit

private func policy(_ json: String) throws -> [[String: Any]] {
    let doc = try #require(try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any])
    return try #require(doc["spaces"] as? [[String: Any]])
}

private func space(_ spaces: [[String: Any]], _ name: String) -> [String: Any]? {
    spaces.first { $0["name"] as? String == name }
}

/// SP-052.1: the proposals the wizard makes on the fixture machine and on
/// the cases around it.
@Suite struct SpacesSetupPlanTests {
    private func plan(_ scan: SpacesScan = fixtureScan(), applied: AppliedPolicy = AppliedPolicy(),
                      kinds: [String: SetupKind] = [:], profiles: [String: String] = [:]) -> (groups: [SetupGroup], plan: SetupPlan) {
        let groups = SpacesSetupPlanner.groups(scan: scan, applied: applied)
        return (groups, SpacesSetupPlanner.plan(groups: groups, kinds: kinds, profiles: profiles, scan: scan, applied: applied,
                                                home: setupHome))
    }

    /// SP-052.1 a1 (supporting the manual check): 2 job orgs, 1 open org
    /// with one private repo and 1 personal account give 4 spaces of the
    /// right kinds, and the private open-org repo goes to quarantine.
    @Test func fixtureMachineProposesFourSpacesAndQuarantine() throws {
        let (groups, plan) = plan()
        #expect(groups.map(\.owner) == ["acme", "globex", "me", "oss-org"])
        #expect(groups.map(\.proposed) == [.job, .job, .personal, .open])
        #expect(plan.newSpaces.map(\.name).sorted() == ["acme", "globex", "personal", "public"])
        #expect(Dictionary(uniqueKeysWithValues: plan.spaces.map { ($0.name, $0.kind) })
            == ["acme": .job, "globex": .job, "personal": .personal, "public": .open])
        #expect(plan.quarantined.map(\.checkout.repo) == ["oss-secret"])
        #expect(plan.quarantined.first?.rule == "not_verified_public")
        #expect(plan.conflicts.isEmpty)
        #expect(plan.placedCounts.map(\.space) == ["acme", "globex", "personal", "public"])
        #expect(plan.placedCounts.map(\.count) == [2, 2, 2, 2])

        let spaces = try policy(plan.policyJSON)
        #expect(spaces.count == 4)
        let acme = try #require(space(spaces, "acme"))
        #expect(acme["kind"] as? String == "isolated")
        #expect(acme["github_orgs"] as? [String] == ["acme"])
        #expect(acme["roots"] as? [String] == ["~/work/acme"])
        #expect(acme["gh_config_dir"] as? String == "~/.config/gh-acme")
        let pub = try #require(space(spaces, "public"))
        #expect(pub["kind"] as? String == "public")
        // D-65: an open space gets no folder roots.
        #expect(pub["roots"] == nil)
        let personal = try #require(space(spaces, "personal"))
        #expect(personal["kind"] as? String == "private")
        #expect(personal["github_orgs"] as? [String] == ["me"])
        // ~/code also holds the open org's private repo, which a personal
        // root would take out of quarantine.
        #expect(personal["roots"] == nil)
        let rejected = try #require(plan.roots.first { $0.space == "personal" })
        #expect(rejected.rejected?.contains("oss-org/oss-secret") == true)
        #expect(personal["gh_config_dir"] == nil)
    }

    @Test func choicesChangeKindsAndProfiles() throws {
        let (_, plan) = plan(kinds: ["oss-org": .leaveOut, "globex": .personal], profiles: ["acme": "/Volumes/x/gh acme"])
        #expect(plan.spaces.map(\.name).sorted() == ["acme", "personal"])
        #expect(plan.spaces.first { $0.name == "personal" }?.orgs == ["globex", "me"])
        #expect(plan.spaces.first { $0.name == "acme" }?.ghConfigDir == "/Volumes/x/gh acme")
        // Left out, the open org binds nothing, so the folder decides: with
        // its private repo no longer an open org's, ~/code can be personal's.
        #expect(plan.spaces.first { $0.name == "personal" }?.roots == ["\(setupHome)/code", "\(setupHome)/work/globex"])
        #expect(plan.quarantined.isEmpty)
    }

    @Test func proposalsFollowTheEvidence() {
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "/x/a/1", owner: "Mixed", repo: "1", visibility: "public"),
            ScannedCheckout(path: "/x/a/2", owner: "Mixed", repo: "2", visibility: "private"),
            ScannedCheckout(path: "/x/b/1", owner: "unknown", repo: "1"),
            ScannedCheckout(path: "/x/c", owner: "", repo: ""),
            ScannedCheckout(path: "/x/d", owner: "anon", repo: "d", visibility: "public", anonymous: true),
        ], accounts: [ScannedAccount(login: "someone", orgs: [])])
        let groups = SpacesSetupPlanner.groups(scan: scan, applied: AppliedPolicy())
        #expect(groups.map(\.id) == ["mixed", "unknown"])
        // Half public is not enough to be open: the stricter kind wins.
        #expect(groups.map(\.proposed) == [.job, .job])
        #expect(groups[0].evidence == "1 public, 1 private")
        #expect(groups[1].evidence == "1 not known")
        #expect(groups[0].notPublic.map(\.repo) == ["2"])
    }

    @Test func evidenceForPeople() {
        let (groups, _) = plan()
        #expect(groups.map(\.evidence) == ["all repos private · you're a member", "all repos private · you're a member",
                                           "1 public, 1 private · your own account", "2 public, 1 private · you're a member"])
        let one = SetupGroup(id: "x", owner: "x", checkouts: [], publicRepos: 1, privateRepos: 0, unknownRepos: 0,
                             member: false, own: false, boundSpace: nil, proposed: .open)
        #expect(one.evidence == "public repo")
        var priv = one
        (priv.publicRepos, priv.privateRepos) = (0, 1)
        #expect(priv.evidence == "private repo")
    }

    @Test func aJobRootNeverHoldsAnotherJob() {
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "\(setupHome)/work/a1", owner: "acme", repo: "a1", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/sub/a2", owner: "acme", repo: "a2", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/g1", owner: "globex", repo: "g1", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/fork", owner: "oss", repo: "fork", visibility: "public"),
        ])
        let (_, plan) = plan(scan, kinds: ["oss": .open])
        let acme = plan.roots.first { $0.space == "acme" }
        #expect(acme?.rejected?.contains("globex") == true)
        #expect(plan.spaces.allSatisfy { $0.roots.isEmpty || $0.name == "globex" })
        // globex's lone checkout's folder holds acme's too.
        #expect(plan.roots.first { $0.space == "globex" }?.rejected != nil)
    }

    @Test func aJobRootMayHoldPublicForksAndLocalRepos() {
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "\(setupHome)/work/acme/api", owner: "acme", repo: "api", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/acme/web", owner: "acme", repo: "web", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/acme/oss-fork", owner: "oss", repo: "fork", visibility: "public"),
            ScannedCheckout(path: "\(setupHome)/work/acme/scratch"),
        ], accounts: [ScannedAccount(login: "me", orgs: ["acme"])])
        let (_, plan) = plan(scan, kinds: ["oss": .open])
        #expect(plan.spaces.first { $0.name == "acme" }?.roots == ["\(setupHome)/work/acme"])
        let byRepo = Dictionary(uniqueKeysWithValues: plan.placements.map { ($0.checkout.path, ($0.space, $0.rule)) })
        #expect(byRepo["\(setupHome)/work/acme/oss-fork"]! == ("acme", "root_stricter"))
        #expect(byRepo["\(setupHome)/work/acme/scratch"]! == ("acme", "root"))
        #expect(byRepo["\(setupHome)/work/acme/api"]! == ("acme", "root_and_remote_agree"))
        #expect(plan.roots.first?.checkouts == 4)
    }

    /// A job's root never pulls the user's own or a left-out owner's repos
    /// into the employer's space, and a truncated scan proposes no roots.
    @Test func aJobRootNeverTakesPersonalOrLeftOutRepos() {
        let flat = SpacesScan(checkouts: [
            ScannedCheckout(path: "\(setupHome)/code/a1", owner: "acme", repo: "a1", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/code/a2", owner: "acme", repo: "a2", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/code/mine", owner: "me", repo: "mine", visibility: "private"),
        ], accounts: [ScannedAccount(login: "me", orgs: ["acme"])])
        let (_, personal) = plan(flat)
        #expect(personal.roots.first { $0.space == "acme" }?.rejected?.contains("would join this job") == true)
        #expect(personal.placements.first { $0.checkout.repo == "mine" }?.space == "personal")
        let (_, left) = plan(flat, kinds: ["me": .leaveOut])
        #expect(left.roots.first { $0.space == "acme" }?.rejected?.contains("would join this job") == true)
        #expect(left.placements.first { $0.checkout.repo == "mine" }?.outcome == "quarantined")

        var truncated = fixtureScan()
        truncated.truncated = true
        let (_, cut) = plan(truncated)
        #expect(cut.roots.allSatisfy { $0.rejected?.contains("stopped early") == true })
        #expect(cut.spaces.allSatisfy { $0.roots.isEmpty })
    }

    @Test func rootsNeverAtOrAboveHome() {
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "\(setupHome)/a", owner: "acme", repo: "a", visibility: "private"),
            ScannedCheckout(path: "/opt/b", owner: "globex", repo: "b", visibility: "private"),
            ScannedCheckout(path: "/opt/c", owner: "globex", repo: "c", visibility: "private"),
        ])
        let (_, plan) = plan(scan)
        #expect(plan.roots.first { $0.space == "acme" }?.rejected?.contains("home") == true)
        #expect(plan.roots.first { $0.space == "globex" }?.rejected?.contains("home") == true)
        #expect(plan.spaces.allSatisfy { $0.roots.isEmpty })
    }

    /// INV-14 against the applied policy: a proposed root inside another
    /// job's folder, or one around a stricter space's, is not bound.
    @Test func nestingFollowsTheStricterOnlyRule() throws {
        let applied = try AppliedPolicy.parse("""
        {"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"]},
                   {"name":"home","kind":"private","roots":["~/code"]}]}
        """)
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "\(setupHome)/work/acme/x/g1", owner: "globex", repo: "g1", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/acme/x/g2", owner: "globex", repo: "g2", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/code/p/1", owner: "pal", repo: "1", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/code/p/2", owner: "pal", repo: "2", visibility: "private"),
            ScannedCheckout(path: "\(setupHome)/work/acme/api", owner: "acme", repo: "api", visibility: "private"),
        ])
        let (groups, plan) = plan(scan, applied: applied, kinds: ["pal": .personal])
        #expect(groups.first { $0.id == "acme" }?.locked == true)
        let globex = try #require(plan.roots.first { $0.space == "globex" })
        #expect(globex.rejected?.contains("inside job acme") == true)
        #expect(globex.depth == 1)
        // pal joins the applied private space and its folder nests inside it.
        let pal = try #require(plan.roots.first { $0.space == "home" && !$0.existing })
        #expect(pal.rejected == nil)
        #expect(pal.depth == 1)
        #expect(plan.spaces.map(\.name).sorted() == ["globex", "home"])
        #expect(plan.conflicts.map(\.checkout.repo) == ["g1", "g2"])

        let outer = SetupRoot(path: "/w", space: "p", kind: "private", existing: false, rejected: nil, depth: 0, checkouts: 0)
        let inner = SetupRoot(path: "/w/j", space: "j", kind: "isolated", existing: true, rejected: nil, depth: 0, checkouts: 0)
        let pub = SetupRoot(path: "/w/j/k", space: "o", kind: "public", existing: false, rejected: nil, depth: 0, checkouts: 0)
        let same = SetupRoot(path: "/W/J", space: "q", kind: "isolated", existing: false, rejected: nil, depth: 0, checkouts: 0)
        let wrap = SetupRoot(path: "/", space: "z", kind: "isolated", existing: false, rejected: nil, depth: 0, checkouts: 0)
        let nested = SpacesSetupPlanner.nest(existing: [inner], proposed: [outer, pub, same, wrap])
        #expect(nested.first { $0.space == "p" }?.rejected == nil)
        #expect(nested.first { $0.space == "o" }?.rejected?.contains("less strict") == true)
        #expect(nested.first { $0.space == "q" }?.rejected?.contains("already bound") == true)
        #expect(nested.first { $0.space == "z" }?.rejected?.contains("would hold") == true)
        let looser = SetupRoot(path: "/w/j/deep", space: "pp", kind: "public", existing: true, rejected: nil, depth: 0, checkouts: 0)
        let around = SetupRoot(path: "/w/j/", space: "r", kind: "private", existing: false, rejected: nil, depth: 0, checkouts: 0)
        #expect(SpacesSetupPlanner.nestProblem(around, among: [looser])?.contains("less strict") == true)
    }

    /// A rerun only proposes changes: a bound owner stays as it is, and the
    /// fields setup does not know survive.
    @Test func rerunKeepsTheAppliedPolicy() throws {
        let appliedJSON = """
        {"public_root_sessions":"me","spaces":[{"name":"public","kind":"public","github_orgs":["oss-org"],"web_prefixes":["https://go.dev/"],"public_paths":["~/code/oss"]},
                   {"name":"me","kind":"private","github_orgs":["ME"],"claude_config_dir":"~/.claude-me"}]}
        """
        let applied = try AppliedPolicy.parse(appliedJSON)
        let (groups, plan) = plan(applied: applied)
        #expect(groups.filter(\.locked).map(\.owner) == ["me", "oss-org"])
        #expect(plan.newSpaces.map(\.name).sorted() == ["acme", "globex"])
        let spaces = try policy(plan.policyJSON)
        let pub = try #require(space(spaces, "public"))
        #expect(pub["web_prefixes"] as? [String] == ["https://go.dev/"])
        #expect(pub["public_paths"] as? [String] == ["~/code/oss"])
        #expect(space(spaces, "me")?["claude_config_dir"] as? String == "~/.claude-me")
        let doc = try #require(try JSONSerialization.jsonObject(with: Data(plan.policyJSON.utf8)) as? [String: Any])
        #expect(doc["public_root_sessions"] as? String == "me")

        // Nothing new: nothing changes.
        var scan = fixtureScan()
        scan.checkouts.removeAll { $0.owner == "acme" || $0.owner == "globex" }
        let (_, same) = self.plan(scan, applied: applied)
        #expect(!same.changesPolicy)
        #expect(same.spaces.isEmpty)
    }

    @Test func newOrgsJoinAnAppliedSpaceOfTheirKind() throws {
        let applied = try AppliedPolicy.parse("""
        {"spaces":[{"name":"oss","kind":"public","github_orgs":["other"]},{"name":"mine","kind":"private"}]}
        """)
        let (_, plan) = plan(applied: applied)
        #expect(plan.spaces.first { $0.name == "oss" }?.isNew == false)
        #expect(plan.spaces.first { $0.name == "oss" }?.orgs == ["oss-org"])
        #expect(plan.spaces.first { $0.name == "mine" }?.orgs == ["me"])
        let spaces = try policy(plan.policyJSON)
        #expect(space(spaces, "oss")?["github_orgs"] as? [String] == ["other", "oss-org"])
        #expect(spaces.count == 4)
    }

    @Test func spaceNamesAreValidAndUnique() {
        #expect(SpacesSetupPlanner.spaceName("Acme.Corp") == "acme-corp")
        #expect(SpacesSetupPlanner.spaceName("42labs") == "org-42labs")
        #expect(SpacesSetupPlanner.spaceName(String(repeating: "a", count: 40)).count == 32)
        var taken: Set<String> = ["public", "acme"]
        #expect(SpacesSetupPlanner.unique("acme", taken: &taken) == "acme-2")
        #expect(SpacesSetupPlanner.unique("acme", taken: &taken) == "acme-3")
        #expect(SpacesSetupPlanner.unique(String(repeating: "b", count: 32), taken: &taken).count == 32)
        var long: Set<String> = [String(repeating: "b", count: 32)]
        #expect(SpacesSetupPlanner.unique(String(repeating: "b", count: 32), taken: &long) == String(repeating: "b", count: 30) + "-2")

        // An org called "public" does not take the open space's name.
        let scan = SpacesScan(checkouts: [
            ScannedCheckout(path: "/x/p", owner: "public", repo: "p", visibility: "private"),
            ScannedCheckout(path: "/x/o", owner: "oss", repo: "o", visibility: "public"),
        ])
        let groups = SpacesSetupPlanner.groups(scan: scan, applied: AppliedPolicy())
        let plan = SpacesSetupPlanner.plan(groups: groups, kinds: [:], profiles: [:], scan: scan, applied: AppliedPolicy(), home: setupHome)
        #expect(Set(plan.spaces.map(\.name)).count == 2)
    }

    @Test func placementMirrorsTheClassifier() {
        let roots = [
            SetupRoot(path: "/c", space: "pub", kind: "public", existing: true, rejected: nil, depth: 0, checkouts: 0),
            SetupRoot(path: "/w", space: "a", kind: "isolated", existing: true, rejected: nil, depth: 0, checkouts: 0),
            SetupRoot(path: "/p", space: "me", kind: "private", existing: true, rejected: nil, depth: 0, checkouts: 0),
        ]
        let orgs: [String: (String, String)] = ["g": ("g", "isolated"), "o": ("pub", "public"), "a": ("a", "isolated")]
        func at(_ path: String, _ owner: String = "", _ vis: String = "") -> SetupPlacement {
            SpacesSetupPlanner.place(ScannedCheckout(path: path, owner: owner, repo: "r", visibility: vis), roots: roots, orgs: orgs)
        }
        #expect(at("/w/x", "g").outcome == "conflict")
        #expect(at("/p/x", "g").rule == "remote_stricter")
        #expect(at("/c/x", "o", "public").rule == "no_public_proof")
        #expect(at("/c/x", "o", "private").rule == "no_public_proof")
        #expect(at("/c/x").rule == "no_public_proof")
        #expect(at("/elsewhere", "o", "public").space == "pub")
        #expect(at("/elsewhere", "o", "private").rule == "not_verified_public")
        #expect(at("/elsewhere", "nobody").rule == "unmatched")
        #expect(at("/p/x", "o", "public").rule == "root_stricter")
        #expect(at("/w/x", "a").rule == "root_and_remote_agree")
    }

    @Test func pathHelpers() {
        #expect(SpacesSetupPlanner.commonAncestor([]) == nil)
        #expect(SpacesSetupPlanner.commonAncestor(["/a/b/c", "/A/b/d"]) == "/a/b")
        #expect(SpacesSetupPlanner.commonAncestor(["/a", "/b"]) == "/")
        #expect(SpacesSetupPlanner.parent("x") == "/")
        #expect(SpacesSetupPlanner.isUnder("/a/bc", "/a/b") == false)
        #expect(SpacesSetupPlanner.isUnder("/a/b/c", "/a/b/") == true)
        #expect(SpacesSetupPlanner.expand("~", home: "/h") == "/h")
        #expect(SpacesSetupPlanner.expand("~/x", home: "/h") == "/h/x")
        #expect(SpacesSetupPlanner.abbreviated("/h", home: "/h") == "~")
        #expect(SpacesSetupPlanner.abbreviated("/h/x", home: "/h") == "~/x")
        #expect(SpacesSetupPlanner.abbreviated("/other", home: "/h") == "/other")
        #expect(SpacesSetupPlanner.abbreviated("/x", home: "/") == "/x")
        // An applied document setup cannot read is never replaced.
        #expect(SpacesSetupPlanner.policyJSON(applied: "not json", spaces: [], home: "/h") == nil)
        #expect(SpacesSetupPlanner.policyJSON(applied: "{\"spaces\":3}", spaces: [], home: "/h") == nil)
        #expect(SpacesSetupPlanner.policyJSON(applied: "{}", spaces: [], home: "/h") == "{\"spaces\":[]}")
        #expect(throws: (any Error).self) { try AppliedPolicy.parse("{") }
        #expect(SetupKind(policyKind: "nope") == nil)
        #expect(SetupKind.allCases.map(\.title) == ["Job: keep separate", "Personal", "Open", "Leave out"])
        #expect(SetupKind.allCases.map(\.shortTitle) == ["Job", "Personal", "Open", "Leave out"])
        #expect(SetupKind.leaveOut.policyKind == nil)
        #expect(SpacesSetupPlanner.strictness("weird") == 0)
    }
}
