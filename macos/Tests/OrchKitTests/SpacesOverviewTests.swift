import Foundation
import GRPCCore
import GRPCProtobuf
import Testing
@testable import OrchKit

private let home = "/Users/me"
private let epoch = Date(timeIntervalSince1970: 1_800_000_000)

/// Four spaces like the design's: acme and globex jobs, a personal space
/// and a public one. Every name, path, org and count of a space other than
/// acme is distinct, so screen-share-safe mode can be checked for leaks by
/// searching for them.
private func fixtureListing(drift: [String] = []) -> SpacesListing {
    SpacesListing(
        status: SpacesPolicyStatus(configured: true, appliedRevision: 7, draftRevision: drift.contains("draft") ? 8 : 0,
                                   drift: drift),
        spaces: [
            SpaceInfo(name: "acme", kind: "isolated", githubOrgs: ["acme"], roots: ["\(home)/work/acme"],
                      chat: ["slack:T0ACME"], reads: ["personal"], readable: ["acme", "opensrc", "personal"],
                      termsFile: "\(home)/.config/orch/terms/acme.txt", ghConfigDir: "\(home)/.config/gh-acme"),
            SpaceInfo(name: "globex", kind: "isolated", githubOrgs: ["globex-corp"], roots: ["\(home)/work/globex"],
                      webPrefixes: ["https://wiki.globex.example/"], chat: ["teams:globex-tenant"],
                      readable: ["globex", "opensrc"], termsFile: "\(home)/.config/orch/terms/globex.txt",
                      ghConfigDir: "\(home)/.config/gh-globex"),
            SpaceInfo(name: "personal", kind: "private", githubOrgs: ["willesq"], roots: ["\(home)/code/willesq"],
                      readable: ["personal", "opensrc"]),
            SpaceInfo(name: "opensrc", kind: "public", githubOrgs: ["opensrc-org"], roots: ["\(home)/code"],
                      webPrefixes: ["https://go.dev/"], readable: ["opensrc"]),
        ]
    )
}

private func fixtureSessions() -> [PinnedSession] {
    [
        PinnedSession(harness: "claude", sessionID: "4f2a1b2c-acme", harnessPID: 101, space: "acme",
                      firstCwd: "\(home)/work/acme/api", state: "active", lastActivity: epoch, toolEvents: 11, refusals: 0),
        PinnedSession(harness: "codex", sessionID: "9b11aaaa-acme", harnessPID: 102, space: "acme",
                      firstCwd: "\(home)/work/acme/web", state: "idle", toolEvents: 12, refusals: 1),
        PinnedSession(harness: "claude", sessionID: "77c0globex", harnessPID: 7703, space: "globex",
                      firstCwd: "\(home)/work/globex/core", state: "active", toolEvents: 7301, refusals: 7302),
        PinnedSession(harness: "claude", sessionID: "c0nf1ict", harnessPID: 7704, space: "quarantine",
                      firstCwd: "\(home)/work/acme/billing-globex", state: "active", toolEvents: 7305),
        PinnedSession(harness: "codex", sessionID: "qqqq-unplaced", harnessPID: 7706, space: "quarantine",
                      firstCwd: "/Volumes/scratch/unplaced", state: "idle", toolEvents: 7307),
        PinnedSession(harness: "claude", sessionID: "ended-personal", harnessPID: 7708, space: "personal",
                      firstCwd: "\(home)/code/willesq/notes", state: "ended", toolEvents: 7309),
        PinnedSession(harness: "codex", harnessPID: 7710, state: "unpinned"),
    ]
}

private func fixtureActivity() -> [SpaceActivity] {
    [
        SpaceActivity(space: "acme", sessions: 2, toolCalls: 23, directoryMoves: 3, crossSpaceMoves: 1, refusals: 1),
        SpaceActivity(space: "globex", sessions: 1, toolCalls: 7311, directoryMoves: 7312, crossSpaceMoves: 7313, refusals: 7314),
        SpaceActivity(space: "personal", sessions: 1, toolCalls: 7321, directoryMoves: 7322, refusals: 0),
        SpaceActivity(space: "", sessions: 1, toolCalls: 7331),
    ]
}

/// Everything that names a space other than acme, or counts one.
private let otherSpaces = [
    "globex", "personal", "opensrc", "willesq", "quarantine", "/work/globex", "/code", "go.dev", "teams:", "billing",
    "/Volumes/scratch", "c0nf1ict", "qqqq", "77c0", "ended-personal", "7703", "7704", "7706", "7708", "7710",
    "7301", "7302", "7305", "7307", "7309", "7311", "7312", "7313", "7314", "7321", "7322", "7331", "7315", "Relabelling",
]

private final class FakeOverview: SpacesOverviewAPI, @unchecked Sendable {
    private let lock = NSLock()
    var listing = fixtureListing()
    var sessions = fixtureSessions()
    var activity = fixtureActivity()
    var orgs = ["globex-corp"]
    /// Folders ExplainPlacement places in a conflict.
    var conflictFolders: Set<String> = ["\(home)/work/acme/billing-globex"]
    var failListing: (any Error)?
    var failSessions: (any Error)?
    var failActivity: (any Error)?
    var failOrgs: (any Error)?
    private(set) var asked: [String] = []
    private(set) var askedEnded: [Bool] = []

    func enforcement() async throws -> EnforcementState { EnforcementState() }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        throw CancellationError()
    }

    func explainPlacement(target: String) async throws -> PlacementExplanation {
        lock.withLock { asked.append(target) }
        if conflictFolders.contains(target) {
            return PlacementExplanation(rule: "conflict", reason: "acme root, globex remote", outcome: "conflict",
                                        space: "quarantine")
        }
        return PlacementExplanation(rule: "unmatched", outcome: "quarantined", space: "quarantine")
    }

    func listSpaces() async throws -> SpacesListing {
        try lock.withLock {
            if let failListing { throw failListing }
            return listing
        }
    }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        try lock.withLock {
            askedEnded.append(includeEnded)
            if let failSessions { throw failSessions }
            return includeEnded ? sessions : sessions.filter { $0.state != "ended" }
        }
    }

    func toolLogSummary() async throws -> [SpaceActivity] {
        try lock.withLock {
            if let failActivity { throw failActivity }
            return activity
        }
    }

    func unconfirmedGitHubOrgs() async throws -> [String] {
        try lock.withLock {
            if let failOrgs { throw failOrgs }
            return orgs
        }
    }
}

private struct Down: Error, CustomStringConvertible {
    var description: String { "orchd is not running" }
}

@MainActor
private func freshDefaults() -> UserDefaults {
    let name = "orch-spaces-overview-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

/// Every string and number a view could show from the model.
@MainActor
private func everything(_ model: SpacesOverviewModel) -> String {
    var out = ""
    dump(model.overview, to: &out)
    dump(model.spaceChoices, to: &out)
    dump(model.badge, to: &out)
    dump(model.badge.label, to: &out)
    dump(model.lastError, to: &out)
    dump(model.shareSafe, to: &out)
    return out
}

@Suite struct SpacesBadgeTests {
    /// SP-051.1 a2: conflicts > quarantine > drift > relabel.
    @Test func badgeFollowsConflictsQuarantineDriftRelabel() {
        let relabel = RelabelProgress(done: 40, total: 100)
        #expect(SpacesBadge.pick(configured: true, conflicts: 2, quarantine: 5, drift: ["draft"], relabel: relabel) == .conflicts(2))
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 5, drift: ["draft"], relabel: relabel) == .quarantine(5))
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 0, drift: ["draft"], relabel: relabel) == .drift(["draft"]))
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 0, drift: [], relabel: relabel) == .relabel(relabel))
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 0, drift: [], relabel: nil) == .none)
        // A finished or empty relabel is not progress.
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 0, drift: [],
                                 relabel: RelabelProgress(done: 100, total: 100)) == .none)
        #expect(SpacesBadge.pick(configured: true, conflicts: 0, quarantine: 0, drift: [],
                                 relabel: RelabelProgress(done: 0, total: 0)) == .none)
        // With nothing applied the badge says so; a draft still outranks it.
        #expect(SpacesBadge.pick(configured: false, conflicts: 0, quarantine: 0, drift: [], relabel: nil) == .notSetUp)
        #expect(SpacesBadge.pick(configured: false, conflicts: 0, quarantine: 0, drift: ["draft"], relabel: nil) == .drift(["draft"]))
        // Every combination of the four: the first present in the order wins.
        for mask in 0..<16 {
            let c = mask & 1 != 0, q = mask & 2 != 0, d = mask & 4 != 0, r = mask & 8 != 0
            let badge = SpacesBadge.pick(configured: true, conflicts: c ? 1 : 0, quarantine: q ? 3 : 0,
                                         drift: d ? ["changed_outside_orchd"] : [], relabel: r ? relabel : nil)
            let want: SpacesBadge = c ? .conflicts(1) : q ? .quarantine(3) : d ? .drift(["changed_outside_orchd"])
                : r ? .relabel(relabel) : .none
            #expect(badge == want, "mask \(mask)")
        }
    }

    @Test func badgeLabels() {
        #expect(SpacesBadge.none.label == "")
        #expect(SpacesBadge.notSetUp.label == "Not set up")
        #expect(SpacesBadge.conflicts(1).label == "1 conflict")
        #expect(SpacesBadge.conflicts(3).label == "3 conflicts")
        #expect(SpacesBadge.quarantine(1).label == "1 item in quarantine")
        #expect(SpacesBadge.quarantine(14).label == "14 items in quarantine")
        #expect(SpacesBadge.drift(["draft", "changed_outside_orchd"]).label
            == "A saved draft is not applied; The applied policy was changed outside orchd")
        #expect(SpacesBadge.relabel(RelabelProgress(done: 1, total: 3)).label == "Relabelling: 33%")
        #expect(RelabelProgress(done: 5, total: 3).percent == 100)
        #expect(RelabelProgress(done: 5, total: 0).percent == 0)
    }

    /// The badge built from orchd's answers takes the same order.
    @Test func overviewBadgeFromData() {
        func badge(conflicted: Set<String>, orgs: [String], drift: [String], sessions: [PinnedSession] = fixtureSessions(),
                   relabel: RelabelProgress? = nil) -> SpacesBadge {
            SpacesOverview.build(listing: fixtureListing(drift: drift), sessions: sessions, activity: [],
                                 conflicted: conflicted, unconfirmedOrgs: orgs, relabel: relabel, detail: .basic, home: home).badge
        }
        #expect(badge(conflicted: ["c0nf1ict"], orgs: ["globex-corp"], drift: ["draft"]) == .conflicts(1))
        // Without the conflict, both quarantined sessions and the unconfirmed org count.
        #expect(badge(conflicted: [], orgs: ["globex-corp"], drift: ["draft"]) == .quarantine(3))
        let calm = fixtureSessions().filter { $0.space != "quarantine" }
        #expect(badge(conflicted: [], orgs: [], drift: ["draft"], sessions: calm) == .drift(["draft"]))
        #expect(badge(conflicted: [], orgs: [], drift: [], sessions: calm, relabel: RelabelProgress(done: 2, total: 9))
            == .relabel(RelabelProgress(done: 2, total: 9)))
        #expect(badge(conflicted: [], orgs: [], drift: [], sessions: calm) == .none)
        // An ended session in quarantine needs nothing.
        var ended = calm
        ended.append(PinnedSession(harness: "codex", sessionID: "gone", space: "quarantine", firstCwd: "/x", state: "ended"))
        #expect(badge(conflicted: [], orgs: [], drift: [], sessions: ended) == .none)
    }
}

@MainActor
@Suite struct SpacesOverviewTests {
    @Test func buildsCardsSessionsAndNeedsYou() {
        let o = SpacesOverview.build(listing: fixtureListing(drift: ["draft"]), sessions: fixtureSessions(),
                                     activity: fixtureActivity(), conflicted: ["c0nf1ict"], unconfirmedOrgs: ["globex-corp"],
                                     relabel: RelabelProgress(done: 1, total: 4), detail: .basic, home: home)
        #expect(o.configured && o.appliedRevision == 7 && o.draftRevision == 8)
        #expect(o.cards.map(\.name) == ["acme", "globex", "personal", "opensrc"])
        let acme = o.cards[0]
        #expect(acme.kindLabel == "Isolated · job")
        #expect(acme.bindings == [SpaceBinding(label: "GitHub", values: ["acme"]),
                                  SpaceBinding(label: "Folders", values: ["~/work/acme"])])
        #expect(acme.reads.isEmpty, "Basic leaves reads out")
        #expect(acme.liveSessions == 2 && acme.toolCalls == 23 && acme.directoryMoves == 3 && acme.crossSpaceMoves == 1)
        #expect(acme.refusals == 1)
        #expect(o.cards[2].kindLabel == "Private · personal" && o.cards[3].kindLabel == "Public · readable everywhere")
        #expect(o.cards[2].toolCalls == 7321 && o.cards[3].toolCalls == 0)
        // Basic: ended sessions stay out; the unpinned process shows.
        #expect(!o.sessions.contains { $0.state == "ended" })
        #expect(o.sessions.first?.title == "Claude Code · 4f2a1b2c" && o.sessions.first?.directory == "~/work/acme/api")
        #expect(o.sessions.last?.title == "Codex · PID 7710" && o.sessions.last?.id == "pid:7710")
        #expect(o.needsYou.map(\.kind) == [.conflict, .quarantine, .drift, .relabel, .refusals])
        #expect(o.needsYou[0].details == ["~/work/acme/billing-globex"])
        #expect(o.needsYou[1].title == "2 items in quarantine")
        #expect(o.needsYou[1].details.count == 2 && o.needsYou[1].details[1].contains("globex-corp"))
        #expect(o.needsYou[2].details == ["A saved draft is not applied"])
        #expect(o.needsYou[3].title == "Relabelling stored content: 25%")
        #expect(o.needsYou[4].title == "7315 refused tool calls in the last 7 days")
        #expect(o.badge == .conflicts(1))
    }

    @Test func advancedShowsEveryBindingAndEndedSessions() {
        let o = SpacesOverview.build(listing: fixtureListing(), sessions: fixtureSessions(), activity: [],
                                     conflicted: [], unconfirmedOrgs: [], relabel: nil, detail: .advanced, home: home)
        let acme = o.cards[0]
        #expect(acme.bindings.map(\.label) == ["GitHub", "Folders", "Chat", "gh profile", "Outbound terms"])
        #expect(acme.bindings[3].values == ["~/.config/gh-acme"])
        #expect(acme.reads == ["personal"])
        #expect(o.cards[1].bindings.map(\.label).contains("Web"))
        #expect(o.sessions.contains { $0.state == "ended" })
    }

    @Test func singularTextsDriftKindsAndPaths() {
        let one = [PinnedSession(harness: "gemini", sessionID: "c", space: "quarantine", firstCwd: "/a", state: "active"),
                   PinnedSession(harness: "x\u{202E}y", sessionID: "q", space: "quarantine", firstCwd: home, state: "idle")]
        let listing = SpacesListing(status: SpacesPolicyStatus(configured: true, drift: ["changed_outside_orchd", "odd"],
                                                               error: "broken"), spaces: [])
        let o = SpacesOverview.build(listing: listing, sessions: one, activity: [SpaceActivity(space: "a", refusals: 1)],
                                     conflicted: ["c"], unconfirmedOrgs: [], relabel: nil, detail: .basic, home: home)
        #expect(o.needsYou[0].title == "1 session in a conflict: nothing there is indexed")
        #expect(o.needsYou[1].title == "1 item in quarantine" && o.needsYou[1].details == ["~ (session reads nothing)"])
        #expect(o.needsYou[2].details == ["The applied policy was changed outside orchd", "odd",
                                          "The applied policy cannot be read"])
        #expect(o.needsYou[3].title == "1 refused tool call in the last 7 days")
        #expect(o.sessions[0].title.hasPrefix("Gemini · "))
        #expect(!o.sessions[1].title.contains("\u{202E}"), "orchd's strings are made safe to show")
        #expect(SpacesOverview.kindLabel("other") == "other")
        #expect(SpacesOverview.abbreviate("/Users/meet/x", home: home) == "/Users/meet/x")
        #expect(SpacesOverview.abbreviate("/x", home: "/") == "/x")
        #expect(SpacesOverview.abbreviate("/x", home: "") == "/x")
    }

    /// SP-051.1 a3: in screen-share-safe mode no view model holds another
    /// space's names, paths, PR titles or counts.
    @Test func screenShareSafeModeHoldsNothingOfAnotherSpace() async {
        let api = FakeOverview()
        api.listing = fixtureListing(drift: ["draft"])
        let model = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home,
                                        relabel: { RelabelProgress(done: 7316, total: 7317) })
        await model.setDetail(.advanced)
        await model.load()
        // Before: everything shows, so the search below can find leaks.
        let before = everything(model)
        for needle in ["globex", "personal", "7311", "/work/globex", "billing", "Relabelling"] {
            #expect(before.contains(needle), "the fixture shows \(needle) outside safe mode")
        }
        await model.shareOnly("acme")
        let after = everything(model)
        var leaks: [String] = []
        for needle in otherSpaces where after.contains(needle) { leaks.append(needle) }
        #expect(leaks.isEmpty, "occurrences of other spaces in safe mode: \(leaks)")
        // acme's own data still shows.
        let o = model.overview
        #expect(o?.cards.map(\.name) == ["acme"])
        #expect(o?.cards.first?.reads == [])
        #expect(o?.sessions.map(\.id) == ["4f2a1b2c-acme", "9b11aaaa-acme"])
        #expect(o?.needsYou.map(\.kind) == [.drift, .refusals])
        #expect(o?.needsYou.last?.title == "1 refused tool call in the last 7 days")
        #expect(o?.badge == .drift(["draft"]))
        #expect(model.spaceChoices.isEmpty)
        #expect(model.shareSafe == "acme" && !model.allowsNotifications)
        // Safe mode does not even ask about other folders.
        let askedBefore = api.asked.count
        await model.load()
        #expect(api.asked.count == askedBefore)
        #expect(!everything(model).contains("globex"))
        // Back to every space.
        await model.showAll()
        #expect(model.overview?.cards.count == 4 && model.allowsNotifications)
        #expect(model.spaceChoices.map(\.id) == ["acme", "globex", "personal", "opensrc"])
    }

    /// Safe mode filters on orchd's own names, not the display-safe ones,
    /// so two names that look alike once made safe stay apart.
    @Test func screenShareSafeFiltersOnOrchdNames() {
        let tricky = "acme\u{200E}"
        let listing = SpacesListing(status: SpacesPolicyStatus(configured: true), spaces: [
            SpaceInfo(name: "acme", kind: "isolated"), SpaceInfo(name: tricky, kind: "isolated"),
        ])
        let sessions = [PinnedSession(harness: "claude", sessionID: "a", space: "acme", firstCwd: "/a", state: "active"),
                        PinnedSession(harness: "claude", sessionID: "t", space: tricky, firstCwd: "/t", state: "active")]
        let o = SpacesOverview.build(listing: listing, sessions: sessions, activity: [], conflicted: [], unconfirmedOrgs: [],
                                     relabel: nil, detail: .basic, home: home).restricted(to: "acme")
        #expect(o.cards.map(\.id) == ["acme"])
        #expect(o.sessions.map(\.id) == ["a"])
    }

    @Test func screenShareSafeSurvivesARelaunchAndAFailedRead() async {
        let defaults = freshDefaults()
        let api = FakeOverview()
        let first = SpacesOverviewModel(api: api, defaults: defaults, home: home)
        await first.load()
        await first.shareOnly("acme")
        // A relaunch keeps the mode, and its first read is already cut down.
        let second = SpacesOverviewModel(api: api, defaults: defaults, home: home)
        #expect(second.shareSafe == "acme")
        await second.load()
        #expect(second.overview?.cards.map(\.name) == ["acme"])
        // Entering the mode cuts what is loaded at once, even when the next
        // read fails.
        let third = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home)
        await third.load()
        api.failListing = Down()
        await third.shareOnly("acme")
        // orchd's own message could name another space.
        #expect(third.lastError == SpacesOverviewModel.sharingError)
        #expect(!everything(third).contains("globex"))
        #expect(third.overview?.cards.map(\.name) == ["acme"])
        // A chosen space missing from the policy shows nothing.
        api.failListing = nil
        await third.shareOnly("gone")
        #expect(third.overview?.cards.isEmpty == true && third.overview?.sessions.isEmpty == true)
        await third.showAll()
        #expect(defaults.string(forKey: SpacesOverviewModel.shareSafeKey) == "acme")
    }

    @Test func detailIsKeptAndDecidesEndedSessions() async {
        let defaults = freshDefaults()
        let api = FakeOverview()
        let model = SpacesOverviewModel(api: api, defaults: defaults, home: home)
        #expect(model.detail == .basic)
        await model.load()
        await model.setDetail(.basic)
        await model.setDetail(.advanced)
        #expect(api.askedEnded == [false, true])
        #expect(SpacesOverviewModel(api: api, defaults: defaults, home: home).detail == .advanced)
        #expect(SpacesDetail.basic.title == "Basic" && SpacesDetail.advanced.id == "advanced")
    }

    @Test func notConfiguredReadsNothingElse() async {
        let api = FakeOverview()
        api.listing = SpacesListing()
        let model = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home)
        #expect(model.badge == .none)
        await model.load()
        #expect(model.badge == .notSetUp)
        #expect(model.overview?.configured == false)
        #expect(api.askedEnded.isEmpty && api.asked.isEmpty)
    }

    @Test func failedReadsAreReportedNotFatal() async {
        let api = FakeOverview()
        api.failSessions = Down()
        api.failActivity = RPCError(code: .permissionDenied, message: "operator only")
        api.failOrgs = Down()
        let model = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home)
        await model.load()
        #expect(model.overview?.problems == ["Sessions: orchd is not running", "Command log: operator only",
                                             "GitHub orgs: orchd is not running"])
        #expect(model.overview?.cards.count == 4)
        api.failListing = Down()
        await model.load()
        #expect(model.lastError == "orchd is not running")
        #expect(model.overview != nil, "the last answer stays while orchd is away")
        // With no answer yet there is only the error.
        let fresh = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home)
        await fresh.load()
        #expect(fresh.overview == nil && fresh.lastError == "orchd is not running" && !fresh.isLoading)
    }

    @Test func conflictChecksAreCapped() async {
        let api = FakeOverview()
        api.sessions = (0..<30).map {
            PinnedSession(harness: "claude", sessionID: "s\($0)", space: "quarantine", firstCwd: "/q/\($0)", state: "active")
        }
        api.conflictFolders = ["/q/0", "/q/25"]
        let model = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home)
        await model.load()
        #expect(api.asked.count == SpacesOverview.conflictChecks)
        // /q/25 is past the cap, so it counts as quarantine.
        #expect(model.badge == .conflicts(1))
        #expect(model.overview?.needsYou[1].title == "30 items in quarantine")
    }

    @Test func pollsUntilStopped() async throws {
        let api = FakeOverview()
        let model = SpacesOverviewModel(api: api, defaults: freshDefaults(), home: home, interval: .milliseconds(20))
        model.start()
        model.start()
        try await Task.sleep(for: .milliseconds(120))
        model.stop()
        let reads = api.askedEnded.count
        #expect(reads >= 2)
        try await Task.sleep(for: .milliseconds(80))
        #expect(api.askedEnded.count == reads)
    }

}

@Suite struct SpacesOverviewClientTests {
    @Test func daemonClientReadsTheOverview() async throws {
        var listing = Orchd_V1_ListSpacesResponse()
        listing.status.configured = true
        listing.status.appliedRevision = 7
        listing.status.draftRevision = 8
        listing.status.drift = ["draft"]
        listing.status.error = "e"
        var acme = Orchd_V1_SpaceInfo()
        acme.name = "acme"
        acme.kind = "isolated"
        acme.githubOrgs = ["acme"]
        acme.roots = ["/w/acme"]
        acme.webPrefixes = ["https://a/"]
        acme.chat = ["slack:T1"]
        acme.reads = ["personal"]
        acme.readable = ["acme", "personal"]
        acme.termsFile = "/t"
        acme.ghConfigDir = "/g"
        listing.spaces = [acme]
        var sessions = Orchd_V1_ListSessionsResponse()
        var s = Orchd_V1_SessionInfo()
        s.harness = "claude"
        s.sessionID = "abc"
        s.harnessPid = 42
        s.space = "acme"
        s.firstCwd = "/w/acme/x"
        s.state = "active"
        s.lastActivity = 1_800_000_000
        s.toolEvents = 5
        s.refusals = 1
        var unpinned = Orchd_V1_SessionInfo()
        unpinned.harness = "codex"
        unpinned.state = "unpinned"
        sessions.sessions = [s, unpinned]
        var summary = Orchd_V1_ToolLogSummary()
        var line = Orchd_V1_SpaceToolActivity()
        line.space = "acme"
        line.sessions = 2
        line.toolCalls = 9
        line.directoryMoves = 3
        line.crossSpaceMoves = 1
        line.refusals = 1
        summary.spaces = [line]
        var diff = Orchd_V1_SpacesDiff()
        diff.unconfirmedGithubOrgs = ["globex"]
        let service = FakeSpacesOverviewService(listing: listing, sessions: sessions, summary: summary, diff: diff)
        try await withFakeOrchd(services: [service]) { socket in
            let client = try DaemonClient(socketPath: socket)
            let run = Task { try await client.run() }
            defer { client.close(); run.cancel() }
            let l = try await client.listSpaces()
            #expect(l.status == SpacesPolicyStatus(configured: true, appliedRevision: 7, draftRevision: 8, drift: ["draft"], error: "e"))
            #expect(l.spaces == [SpaceInfo(name: "acme", kind: "isolated", githubOrgs: ["acme"], roots: ["/w/acme"],
                                           webPrefixes: ["https://a/"], chat: ["slack:T1"], reads: ["personal"],
                                           readable: ["acme", "personal"], termsFile: "/t", ghConfigDir: "/g")])
            let pins = try await client.listSessions(includeEnded: true)
            #expect(service.recorder.last(Orchd_V1_ListSessionsRequest.self)?.all == true)
            #expect(pins == [PinnedSession(harness: "claude", sessionID: "abc", harnessPID: 42, space: "acme", firstCwd: "/w/acme/x",
                                           state: "active", lastActivity: epoch, toolEvents: 5, refusals: 1),
                             PinnedSession(harness: "codex", state: "unpinned")])
            #expect(try await client.toolLogSummary() == [SpaceActivity(space: "acme", sessions: 2, toolCalls: 9, directoryMoves: 3,
                                                                       crossSpaceMoves: 1, refusals: 1)])
            #expect(try await client.unconfirmedGitHubOrgs() == ["globex"])
        }
    }
}

/// The four reads the Overview makes, served like orchd.
private struct FakeSpacesOverviewService: RegistrableRPCService {
    let listing: Orchd_V1_ListSpacesResponse
    let sessions: Orchd_V1_ListSessionsResponse
    let summary: Orchd_V1_ToolLogSummary
    let diff: Orchd_V1_SpacesDiff
    let recorder = Recorder()

    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        let listing = listing, sessions = sessions, summary = summary, diff = diff, recorder = recorder
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListSpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListSpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListSpacesResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: listing))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.ListSessions.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_ListSessionsRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ListSessionsResponse>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: sessions))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.SummarizeToolLog.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_SummarizeToolLogRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_ToolLogSummary>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: summary))
        }
        router.registerHandler(
            forMethod: Orchd_V1_Orchd.Method.DiffSpaces.descriptor,
            deserializer: ProtobufDeserializer<Orchd_V1_DiffSpacesRequest>(),
            serializer: ProtobufSerializer<Orchd_V1_SpacesDiff>()
        ) { request, _ in
            for try await message in request.messages { recorder.record(message) }
            return StreamingServerResponse(single: ServerResponse(message: diff))
        }
    }
}
