import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private let home = "/Users/me"
private let epoch = Date(timeIntervalSince1970: 1_800_000_000)

/// Serves sessions and a command log per owner, `pageSize` records a page.
private actor FakeActivityAPI: SpacesActivityAPI {
    var sessions: [PinnedSession]
    var logs: [String: [ToolLogEvent]]
    var pageSize: Int
    var reads: [(String, Int64)] = []
    var failSessions: (any Error)?
    var failEvents: (any Error)?

    init(sessions: [PinnedSession], logs: [String: [ToolLogEvent]], pageSize: Int = 2) {
        self.sessions = sessions
        self.logs = logs
        self.pageSize = pageSize
    }

    func set(failSessions: (any Error)? = nil, failEvents: (any Error)? = nil) {
        self.failSessions = failSessions
        self.failEvents = failEvents
    }

    func setSessions(_ s: [PinnedSession]) { sessions = s }

    func listSessions(includeEnded: Bool) async throws -> [PinnedSession] {
        if let failSessions { throw failSessions }
        return sessions
    }

    func toolEvents(owner: String, afterSeq: Int64, limit: Int32) async throws -> ToolLogPage {
        reads.append((owner, afterSeq))
        if let failEvents { throw failEvents }
        let rest = (logs[owner] ?? []).filter { $0.seq > afterSeq }
        let page = Array(rest.prefix(min(pageSize, Int(limit))))
        return ToolLogPage(events: page, more: rest.count > page.count)
    }
}

private func call(_ seq: Int64, _ tool: String = "Bash", command: String = "", decision: String = "logged",
                  rule: String = "") -> ToolLogEvent {
    ToolLogEvent(seq: seq, at: epoch.addingTimeInterval(TimeInterval(seq)), tool: tool, cwd: "\(home)/work/acme/web",
                 command: command, targets: #"{"paths":["/x"]}"#, decision: decision, rule: rule)
}

private let sessions = [
    PinnedSession(harness: "codex", sessionID: "9b11e7a0c4", harnessPID: 2, space: "acme", firstCwd: "\(home)/work/acme/web",
                  state: "active", lastActivity: epoch, toolEvents: 5, refusals: 1),
    PinnedSession(harness: "claude", sessionID: "4f2a1b2c9d", harnessPID: 1, space: "acme", firstCwd: "\(home)/work/acme/api",
                  state: "ended", lastActivity: epoch.addingTimeInterval(-60), toolEvents: 1),
    PinnedSession(harness: "codex", harnessPID: 3, state: "unpinned"),
]

private let codexLog = [
    call(1, command: "npm test"),
    ToolLogEvent(seq: 2, at: epoch, event: "cwd-changed", cwd: "\(home)/work/acme/web", oldCwd: "\(home)/work/acme/web",
                 newCwd: "/tmp/build", decision: "logged"),
    call(3, command: "cd ~/work/globex/core && make", decision: "refused", rule: "other_isolated_root"),
    ToolLogEvent(seq: 4, at: epoch, kind: "gap", event: "", dropped: 3),
    call(5, "Read", decision: "not enforced", rule: "would refuse (other_isolated_root)"),
]

@MainActor
@Suite("Spaces Activity: per-session command log")
struct SpacesActivityTests {
    @Test("lists pinned sessions, newest first, and reads the newest session's whole log, newest record first")
    func loadsLog() async throws {
        let api = FakeActivityAPI(sessions: sessions, logs: ["9b11e7a0c4": codexLog, "4f2a1b2c9d": [call(1)]])
        let model = SpacesActivityModel(api: api, home: home)
        await model.load()
        #expect(model.sessions.map(\.id) == ["9b11e7a0c4", "4f2a1b2c9d"])
        #expect(model.sessions.first?.title == "Codex · 9b11e7a0")
        #expect(model.sessions.first?.directory == "~/work/acme/web")
        #expect(model.selected == "9b11e7a0c4")
        #expect(model.selectedSession?.refusals == 1)
        #expect(model.rows.map(\.id) == [5, 4, 3, 2, 1])
        #expect(!model.more)
        // Pages follow one another by sequence number.
        #expect(await api.reads.map(\.1) == [0, 2, 4])
    }

    @Test("directory moves and refusals are told apart and filtered")
    func filters() async throws {
        let api = FakeActivityAPI(sessions: sessions, logs: ["9b11e7a0c4": codexLog])
        let model = SpacesActivityModel(api: api, home: home)
        await model.load()
        model.filter = .moves
        #expect(model.visibleRows.map(\.id) == [2])
        #expect(model.visibleRows.first?.detail == "~/work/acme/web → /tmp/build")
        model.filter = .refusals
        #expect(model.visibleRows.map(\.id) == [5, 3])
        model.filter = .all
        #expect(model.visibleRows.count == 5)
        #expect(ActivityFilter.allCases.map(\.title) == ["All", "Directory Moves", "Refusals"])
    }

    @Test("rows say what happened and how the guard decided")
    func rows() {
        let refused = ActivityRow.build(codexLog[2], home: home)
        #expect(refused.kind == .call && refused.title == "Bash" && refused.verdict == .refused)
        #expect(refused.detail == "cd ~/work/globex/core && make")
        #expect(refused.decision == "refused · other_isolated_root")
        #expect(refused.directory == "~/work/acme/web")
        let would = ActivityRow.build(codexLog[4], home: home)
        #expect(would.verdict == .wouldRefuse && would.isRefusal)
        // No command: the targets show.
        #expect(would.detail == #"{"paths":["/x"]}"#)
        let gap = ActivityRow.build(codexLog[3], home: home)
        #expect(gap.kind == .gap && gap.detail == "3 events were dropped before they were stored")
        #expect(ActivityRow.build(ToolLogEvent(seq: 9, at: epoch, kind: "gap", dropped: 1), home: home).detail
            == "1 event was dropped before it was stored")
        let end = ActivityRow.build(ToolLogEvent(seq: 10, at: epoch, kind: "end"), home: home)
        #expect(end.kind == .end && end.title == "Session ended" && !end.isRefusal)
        let plain = ActivityRow.build(ToolLogEvent(seq: 11, at: epoch, decision: "not enforced"), home: home)
        #expect(plain.verdict == .notEnforced && plain.title == "Tool call" && plain.decision == "not enforced")
        let bare = ActivityRow.build(ToolLogEvent(seq: 12, at: epoch, rule: "x"), home: home)
        #expect(bare.verdict == .none && bare.decision == "x")
        #expect(ActivityRow.build(call(1), home: home).verdict == .logged)
    }

    @Test("a long log is read a few pages at a time, then on request")
    func loadMore() async throws {
        let long = (1...14).map { call(Int64($0)) }
        let api = FakeActivityAPI(sessions: sessions, logs: ["9b11e7a0c4": long], pageSize: 2)
        let model = SpacesActivityModel(api: api, home: home)
        await model.load()
        #expect(model.rows.count == SpacesActivityModel.pagesPerRead * 2)
        #expect(model.more)
        #expect(model.rows.first?.id == 10)
        await model.loadMore()
        #expect(model.rows.map(\.id) == Array((1...14).reversed()))
        #expect(!model.more)
        await model.loadMore()
        #expect(model.rows.count == 14)
    }

    @Test("choosing a session reads its log; an unknown or unchanged choice does nothing")
    func select() async throws {
        let api = FakeActivityAPI(sessions: sessions, logs: ["9b11e7a0c4": codexLog, "4f2a1b2c9d": [call(1, command: "ls")]])
        let model = SpacesActivityModel(api: api, home: home)
        await model.load()
        await model.select("4f2a1b2c9d")
        #expect(model.rows.map(\.detail) == ["ls"])
        let reads = await api.reads.count
        await model.select("4f2a1b2c9d")
        await model.select("nope")
        #expect(await api.reads.count == reads)
        // The choice is kept across reloads while it is listed.
        await model.load()
        #expect(model.selected == "4f2a1b2c9d")
        await api.setSessions([sessions[0]])
        await model.load()
        #expect(model.selected == "9b11e7a0c4")
    }

    @Test("no sessions, and orchd's refusals")
    func emptyAndErrors() async throws {
        let api = FakeActivityAPI(sessions: [sessions[2]], logs: [:])
        let model = SpacesActivityModel(api: api, home: home)
        await model.load()
        #expect(model.sessions.isEmpty && model.selected == nil && model.rows.isEmpty)
        await api.set(failSessions: RPCError(code: .permissionDenied, message: "only the operator can list sessions"))
        await model.load()
        #expect(model.lastError == "only the operator can list sessions")
        await api.setSessions(sessions)
        await api.set(failEvents: RPCError(code: .permissionDenied, message: "INV-12: not your session"))
        await model.load()
        #expect(model.lastError == "INV-12: not your session")
        #expect(model.rows.isEmpty)
    }
}
