import Foundation
import Testing
@testable import OrchKit

private let home = "/Users/me"

/// The applied revision: two jobs and a personal space.
private let appliedJSON = """
{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public"]},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
 {"name":"personal","kind":"private","roots":["~/code"]}
]}
"""

/// The design's draft: acme also reads globex (refused), personal gains
/// ~/notes, and a field this app does not know rides along.
private let draftJSON = """
{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public","globex"],"future_field":{"x":1}},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
 {"name":"personal","kind":"private","roots":["~/code","~/notes"]}
],"public_root_sessions":"personal"}
"""

private let readsGlobex = SpacesProblemInfo(rule: "isolated_reads_isolated", space: "acme",
                                            message: #"isolated space "acme" cannot read isolated space "globex"; merge them instead"#)

private func diff(draft: String = draftJSON, problems: [SpacesProblemInfo] = [readsGlobex], revision: Int64 = 8,
                  byOperator: Bool = true, orgs: [String] = []) -> SpacesPolicyDiff {
    SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON, draftRevision: revision, draftJSON: draft,
                     problems: problems, draftCreatedBy: "claude session 4f2a", draftCreatedAt: Date(timeIntervalSince1970: 1),
                     draftByOperator: byOperator, unconfirmedGitHubOrgs: orgs)
}

@Suite("Spaces Review: lines, validation and exclusion")
struct SpacesReviewTests {
    @Test("invalid lines are marked with their rule; every other line is not")
    func invalidLinesMarked() throws {
        let review = SpacesReview.build(diff(), home: home)
        #expect(review.reviewedRevision == 8)
        #expect(review.hasProblems)
        #expect(review.unplaced.isEmpty)
        let bad = try #require(review.invalidLines.first)
        #expect(review.invalidLines.count == 1)
        #expect(bad.text == "reads: globex")
        #expect(bad.space == "acme")
        #expect(bad.change == .added)
        #expect(bad.problems == [readsGlobex])
        #expect(bad.excludable)
        #expect(review.excludableCount == 1)
        let notes = try #require(review.lines.first { $0.text == "roots: ~/notes" })
        #expect(notes.change == .added && !notes.invalid)
        #expect(review.lines.first { $0.text == "reads: public" }?.change == .same)
        #expect(review.lines.first { $0.id == "p/public_root_sessions" }?.change == .added)
    }

    @Test("excluding drops only the invalid line and keeps every other field")
    func excludingDropsInvalidLine() throws {
        let doc = try #require(SpacesReview.excluding(diff(), home: home))
        let cleaned = try #require(JSONValue.parse(doc))
        let original = try #require(JSONValue.parse(draftJSON))
        let acme = try #require(cleaned["spaces"]?.arrayValue?.first)
        #expect(acme["reads"] == .array([.string("public")]))
        #expect(acme["future_field"] == .object(["x": .number(1)]))
        // Nothing else changed.
        guard case .object(var o) = original, var spaces = o["spaces"]?.arrayValue, case .object(var a) = spaces[0] else {
            Issue.record("fixture is not an object")
            return
        }
        a["reads"] = .array([.string("public")])
        spaces[0] = .object(a)
        o["spaces"] = .array(spaces)
        #expect(cleaned == .object(o))
        // The cleaned draft has nothing to drop.
        let again = diff(draft: doc, problems: [], revision: 9)
        #expect(SpacesReview.excluding(again, home: home) == nil)
        #expect(!SpacesReview.build(again, home: home).hasProblems)
    }

    @Test("a problem on a line the applied revision already has is marked but not dropped")
    func unchangedLineIsNotExcludable() throws {
        // acme's root is new and holds globex's existing root: dropping
        // globex's root would move its folder into acme.
        let draft = """
        {"spaces":[
         {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme","~/work"],"reads":["public"]},
         {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
         {"name":"personal","kind":"private","roots":["~/code"]}
        ]}
        """
        let p = SpacesProblemInfo(rule: "isolated_root_in_other_isolated", space: "globex",
                                  message: #"root "/Users/me/work/globex" of isolated space "globex" is inside root "/Users/me/work" of isolated space "acme""#)
        let d = diff(draft: draft, problems: [p])
        let review = SpacesReview.build(d, home: home)
        let line = try #require(review.invalidLines.first)
        #expect(line.text == "roots: ~/work/globex")
        #expect(line.change == .same)
        #expect(!line.excludable)
        #expect(review.excludableCount == 0)
        #expect(SpacesReview.excluding(d, home: home) == nil)
    }

    @Test("each rule finds its line", arguments: [
        ("duplicate_name", "acme", #"space "acme" is defined twice"#, "s3"),
        ("bad_name", "Bad", #"space name "Bad" must be 1-32 lowercase letters, digits, - or _, starting with a letter"#, "s4"),
        ("unknown_kind", "odd", #"space "odd" has unknown kind "weird" (want public, private or isolated)"#, "s5/kind"),
        ("org_bound_twice", "personal", #"GitHub org "acme" is bound to both "acme" and "personal""#, "s2/github_orgs/0"),
        ("root_bound_twice", "personal", #"root "/Users/me/work/acme" is bound to both "acme" and "personal""#, "s2/roots/1"),
        ("nested_root_less_strict", "personal", #"root "/Users/me/work/acme/x" (personal, private) is inside "/Users/me/work/acme" (acme, isolated) but less strict"#, "s2/roots/2"),
        ("unknown_read", "personal", #"space "personal" reads "nope", which is not a space"#, "s2/reads/0"),
        ("bad_binding", "personal", #"space "personal" has web prefix "ftp://x", which is not an http or https URL"#, "s2/web_prefixes/0"),
        ("bad_binding", "personal", #"space "personal" has chat tenant "irc:x" (want slack:<id> or teams:<id>)"#, "s2/chat/0"),
        ("bad_binding", "personal", #"space "personal" has terms file "rel.txt", which is not an absolute path"#, "s2/terms_file"),
        ("terms_file_in_git_tree", "personal", #"space "personal" has its terms file inside a git working tree"#, "s2/terms_file"),
        ("gh_config_dir_shared", "personal", #"spaces "acme" and "personal" use the same gh_config_dir; give each its own GitHub account"#, "s2/gh_config_dir"),
        ("claude_config_dir_in_git_tree", "personal", #"space "personal" has its claude_config_dir inside a git working tree"#, "s2/claude_config_dir"),
        ("public_root_sessions", "oss", #"public_root_sessions must name a private or isolated space, not "oss""#, "p/public_root_sessions"),
        ("public_path_outside_root", "personal", #"space "personal": public_paths entry 2 is not inside one of its roots (or a deeper root of another space holds it)"#, "s2/public_paths/1"),
        ("bad_public_path", "personal", #"space "personal": public_paths entry 1 ends with a slash"#, "s2/public_paths/0"),
    ])
    func ruleTargets(rule: String, space: String, message: String, lineID: String) throws {
        let draft = """
        {"spaces":[
         {"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public"]},
         {"name":"globex","kind":"isolated","github_orgs":["globex"],"roots":["~/work/globex"]},
         {"name":"personal","kind":"private","github_orgs":["acme"],"roots":["~/code","~/work/acme","~/work/acme/x"],
          "reads":["nope"],"public_paths":["~/code/a","~/elsewhere"],"web_prefixes":["ftp://x"],"chat":["irc:x"],"terms_file":"rel.txt",
          "gh_config_dir":"/g","claude_config_dir":"/c"},
         {"name":"acme","kind":"isolated"},
         {"name":"Bad","kind":"private"},
         {"name":"odd","kind":"weird"}
        ],"public_root_sessions":"oss"}
        """
        let p = SpacesProblemInfo(rule: rule, space: space, message: message)
        let review = SpacesReview.build(diff(draft: draft, problems: [p]), home: home)
        #expect(review.unplaced.isEmpty)
        #expect(review.invalidLines.map(\.id) == [lineID])
        // A problem between two spaces is the user's choice to settle.
        #expect(review.invalidLines.first?.excludable == !PolicyDoc.conflictRules.contains(rule))
    }

    @Test("a problem between two spaces is marked but never dropped for the user")
    func conflictsAreNotDropped() throws {
        // Both jobs newly claim the same org: dropping either picks a side.
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["shared"],"reads":["nope"]},{"name":"globex","kind":"isolated","github_orgs":["shared"]}]}"#
        let problems = [
            SpacesProblemInfo(rule: "org_bound_twice", space: "globex", message: #"GitHub org "shared" is bound to both "acme" and "globex""#),
            SpacesProblemInfo(rule: "unknown_read", space: "acme", message: #"space "acme" reads "nope", which is not a space"#),
        ]
        let d = SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON, draftRevision: 8, draftJSON: draft, problems: problems)
        let review = SpacesReview.build(d, home: home)
        #expect(review.invalidLines.map(\.id) == ["s0/reads/0", "s1/github_orgs/0"])
        #expect(review.invalidLines.map(\.excludable) == [true, false])
        let cleaned = try #require(SpacesReview.excluding(d, home: home).flatMap(JSONValue.parse))
        #expect(cleaned["spaces"]?.arrayValue?[0]["reads"] == .array([]))
        #expect(cleaned["spaces"]?.arrayValue?[1]["github_orgs"] == .array([.string("shared")]))
    }

    @Test("a binding moved from another space is not dropped: that would leave it unbound")
    func movedBindingsAreNotDropped() throws {
        // The draft takes ~/work/globex from globex and gives it to
        // personal, and orchd refuses that line. Dropping it would leave the
        // folder in no space rather than back in globex.
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public"]},{"name":"globex","kind":"isolated","github_orgs":["globex"]},{"name":"personal","kind":"private","roots":["~/code","~/work/globex/"]}]}"#
        let moved = SpacesProblemInfo(rule: "nested_root_less_strict", space: "personal",
                                      message: #"root "/Users/me/work/globex" (personal, private) is inside "/Users/me/work" (x, isolated) but less strict"#)
        let d = SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON, draftRevision: 8, draftJSON: draft, problems: [moved])
        let review = SpacesReview.build(d, home: home)
        let line = try #require(review.invalidLines.first)
        #expect(line.text == "roots: ~/work/globex/" && line.change == .added && !line.excludable)
        #expect(SpacesReview.excluding(d, home: home) == nil)

        // A renamed space carries the old one's bindings: dropping it would
        // unbind them.
        let renamed = #"{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public"]},{"name":"Globex2","kind":"isolated","github_orgs":["GLOBEX"],"roots":["~/work/globex"]},{"name":"personal","kind":"private","roots":["~/code"]}]}"#
        let bad = SpacesProblemInfo(rule: "bad_name", space: "Globex2", message: #"space name "Globex2" must be lowercase"#)
        let r = SpacesReview.build(SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON, draftRevision: 8,
                                                    draftJSON: renamed, problems: [bad]), home: home)
        #expect(r.invalidLines.map(\.id) == ["s1"])
        #expect(r.excludableCount == 0)
    }

    @Test("a bad binding's field is read right after the space's name, not from inside a value")
    func badBindingPhraseAnchored() {
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["x has root y"],"roots":["x has root y"]}]}"#
        let p = SpacesProblemInfo(rule: "bad_binding", space: "acme",
                                  message: #"space "acme" has GitHub org "x has root y", which is not a GitHub login"#)
        let review = SpacesReview.build(SpacesPolicyDiff(draftRevision: 2, draftJSON: draft, problems: [p]), home: home)
        #expect(review.invalidLines.map(\.id) == ["s0/github_orgs/0"])
        #expect(PolicyDoc.afterFirstQuoted(#"no quotes"#) == "")
        #expect(PolicyDoc.afterFirstQuoted(#"a "b\"c" d"#) == " d")
        #expect(PolicyDoc.afterFirstQuoted(#"a "open"#) == "")
    }

    @Test("problems whose line cannot be found stay on their own and block Apply")
    func unplacedProblems() {
        let problems = [
            SpacesProblemInfo(rule: "too_large", message: "spaces policy is 2000000 bytes"),
            SpacesProblemInfo(rule: "unknown_read", space: "ghost", message: #"space "ghost" reads "x", which is not a space"#),
            SpacesProblemInfo(rule: "bad_binding", space: "acme", message: #"space "acme" has something "x""#),
            SpacesProblemInfo(rule: "unknown_read", space: "acme", message: #"space "acme" reads "absent", which is not a space"#),
            SpacesProblemInfo(rule: "bad_public_path", space: "acme", message: #"space "acme" is isolated; only a public space lists public_paths"#),
            SpacesProblemInfo(rule: "bad_public_path", space: "acme", message: #"space "acme": public_paths entry 9 is malformed"#),
        ]
        let review = SpacesReview.build(diff(problems: problems), home: home)
        #expect(review.unplaced == problems)
        #expect(review.invalidLines.isEmpty)
        #expect(review.hasProblems)
        #expect(SpacesReview.excluding(diff(problems: problems), home: home) == nil)
    }

    @Test("two problems on the same value each claim their own occurrence")
    func repeatedValues() throws {
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","reads":["nope","nope"]}]}"#
        let p = SpacesProblemInfo(rule: "unknown_read", space: "acme", message: #"space "acme" reads "nope", which is not a space"#)
        let d = SpacesPolicyDiff(draftRevision: 2, draftJSON: draft, problems: [p, p])
        let review = SpacesReview.build(d, home: home)
        #expect(review.invalidLines.map(\.id) == ["s0/reads/0", "s0/reads/1"])
        let cleaned = try #require(SpacesReview.excluding(d, home: home).flatMap(JSONValue.parse))
        #expect(cleaned["spaces"]?.arrayValue?.first?["reads"] == .array([]))
    }

    @Test("excluding reverts a changed setting and removes a new space")
    func revertsSettingsAndSpaces() throws {
        let applied = #"{"spaces":[{"name":"acme","kind":"isolated","terms_file":"/t/acme.txt"}],"public_root_sessions":"acme"}"#
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","terms_file":"/repo/acme.txt"},{"name":"Bad","kind":"private"},{"name":"odd","kind":"weird"}],"public_root_sessions":"zzz"}"#
        let problems = [
            SpacesProblemInfo(rule: "terms_file_in_git_tree", space: "acme", message: #"space "acme" has its terms file inside a git working tree"#),
            SpacesProblemInfo(rule: "bad_name", space: "Bad", message: #"space name "Bad" must be lowercase"#),
            SpacesProblemInfo(rule: "unknown_kind", space: "odd", message: #"space "odd" has unknown kind "weird""#),
            SpacesProblemInfo(rule: "public_root_sessions", space: "zzz", message: #"public_root_sessions must name a private or isolated space, not "zzz""#),
        ]
        let d = SpacesPolicyDiff(appliedRevision: 1, appliedJSON: applied, draftRevision: 2, draftJSON: draft, problems: problems)
        let review = SpacesReview.build(d, home: home)
        #expect(review.excludableCount == 4)
        #expect(review.lines.contains { $0.id == "s0/terms_file/-" && $0.change == .removed && $0.text == "terms_file: /t/acme.txt" })
        let cleaned = try #require(SpacesReview.excluding(d, home: home).flatMap(JSONValue.parse))
        #expect(cleaned == JSONValue.parse(applied))
    }

    @Test("a removed space and removed values show as removed lines, and effects count them")
    func removedLinesAndEffects() {
        let draft = #"{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["~/work/acme"],"reads":["public","globex"],"chat":["slack:T1"]},{"name":"newjob","kind":"isolated","roots":["~/work/new"]}]}"#
        let review = SpacesReview.build(diff(draft: draft, problems: [readsGlobex]), home: home)
        let removed = review.lines.filter { $0.change == .removed }.map(\.text)
        #expect(removed == ["globex", "github_orgs: globex", "roots: ~/work/globex", "kind: isolated",
                            "personal", "roots: ~/code", "kind: private"])
        let effects = Dictionary(uniqueKeysWithValues: review.effects.map { ($0.label, $0.count) })
        #expect(effects["Spaces added"] == 1)
        #expect(effects["Spaces removed"] == 2)
        #expect(effects["Spaces changed"] == 1)
        // reads: globex, chat: slack:T1, and newjob's kind and root.
        #expect(effects["Bindings and settings added"] == 4)
        #expect(effects["Bindings and settings removed"] == 5)
        #expect(effects["Invalid lines"] == 1)
    }

    @Test("no pending draft gives nothing to review or apply")
    func noDraft() {
        let review = SpacesReview.build(SpacesPolicyDiff(appliedRevision: 7, appliedJSON: appliedJSON), home: home)
        #expect(review.reviewedRevision == 0)
        #expect(review.lines.isEmpty && !review.hasProblems)
        #expect(SpacesReview.excluding(SpacesPolicyDiff(appliedRevision: 7), home: home) == nil)
    }

    @Test("a draft that is not a policy object is shown raw and cannot be applied")
    func unreadableDraft() {
        let p = SpacesProblemInfo(rule: "unparseable", message: "spaces policy: invalid character")
        let review = SpacesReview.build(SpacesPolicyDiff(draftRevision: 3, draftJSON: "{oops\nline2", problems: [p]), home: home)
        #expect(review.unreadable)
        #expect(review.lines.map(\.text) == ["{oops", "line2"])
        #expect(review.unplaced == [p])
        #expect(review.hasProblems)
        #expect(SpacesReview.excluding(SpacesPolicyDiff(draftRevision: 3, draftJSON: "[1]", problems: [p]), home: home) == nil)
    }

    @Test("Go %q strings are read back, escapes included")
    func goQuoted() {
        #expect(PolicyDoc.goQuoted(#"a "x" b "y\"z" c "\\" "\n\t\r" "\x41é\U0001F600" "open"#)
            == ["x", "y\"z", "\\", "\n\t\r", "Aé😀"])
    }

    @Test("roots expand the way orchd's validator does")
    func expand() {
        #expect(PolicyDoc.expand("~", home: home) == home)
        #expect(PolicyDoc.expand("~/a/./b/../c/", home: home) == "/Users/me/a/c")
        #expect(PolicyDoc.expand("~/a", home: "") == "")
        #expect(PolicyDoc.expand("rel", home: home) == "")
        #expect(PolicyDoc.expand("/..", home: home) == "/")
    }

    @Test("JSON values round-trip every kind")
    func jsonValues() throws {
        let v = try #require(JSONValue.parse(#"{"a":[null,true,1.5,"s",{"b":[]}]}"#))
        #expect(JSONValue.parse(v.text) == v)
        #expect(v["a"]?.arrayValue?.count == 5)
        #expect(JSONValue.bool(true).display == "true")
        #expect(JSONValue.string("x").display == "x")
        #expect(JSONValue.null["k"] == nil && JSONValue.null.arrayValue == nil && JSONValue.null.stringValue == nil)
    }
}
