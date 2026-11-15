import Foundation
@testable import OrchKit

let setupHome = "/Users/me"

/// The SP-052.1 fixture machine: two job orgs (acme, globex), one open org
/// (oss-org) with one private repo, and the user's own account (me).
func fixtureScan() -> SpacesScan {
    let h = setupHome
    return SpacesScan(
        checkouts: [
            ScannedCheckout(path: "\(h)/work/acme/api", owner: "acme", repo: "api", visibility: "private"),
            ScannedCheckout(path: "\(h)/work/acme/web", owner: "acme", repo: "web", visibility: "private"),
            ScannedCheckout(path: "\(h)/work/globex/core", owner: "globex", repo: "core", visibility: "private"),
            ScannedCheckout(path: "\(h)/work/globex/tools", owner: "globex", repo: "tools", visibility: "private"),
            ScannedCheckout(path: "\(h)/code/oss-tools", owner: "oss-org", repo: "oss-tools", visibility: "public"),
            ScannedCheckout(path: "\(h)/code/oss-lib", owner: "oss-org", repo: "oss-lib", visibility: "public"),
            ScannedCheckout(path: "\(h)/code/oss-secret", owner: "oss-org", repo: "oss-secret", visibility: "private"),
            ScannedCheckout(path: "\(h)/code/site", owner: "me", repo: "site", visibility: "public"),
            ScannedCheckout(path: "\(h)/code/notes", owner: "me", repo: "notes", visibility: "private"),
        ],
        accounts: [ScannedAccount(login: "me", orgs: ["acme", "globex", "oss-org"])]
    )
}

/// A fake orchd for setup: it keeps revisions like orch.db, applies only
/// the newest, and records every write.
final class FakeSetupAPI: SpacesSetupAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var revisions: [(json: String, byOperator: Bool)] = []
    private var appliedIndex: Int?
    var scanResult = fixtureScan()
    var switches: [EnforcementSwitch] = [
        EnforcementSwitch(layer: "memory", title: "memory filtering", state: "on", defaultState: "on", states: ["on", "off"]),
        EnforcementSwitch(layer: "guard", title: "tool guard", state: "log", defaultState: "block-other-jobs",
                          states: ["block-other-jobs", "off", "log", "strict"], relaxed: true),
    ]
    var retention = CommandLogRetention(value: "7d")
    var logins: [SpaceLogin] = []
    var problems: [SpacesProblem] = []
    var failApply: (any Error)?
    var failScan: (any Error)?
    var failDiff: (any Error)?
    /// Saves an extra draft right after setup's, as an agent would.
    var agentDraftAfterSave = false
    private(set) var scannedFolders: [[String]] = []
    private(set) var enforcementCalls: [(String, String)] = []
    private(set) var retentionCalls: [String] = []
    private(set) var applyCalls: [Int64] = []

    init(applied: String? = nil, pendingDraft: String? = nil) {
        if let applied {
            revisions.append((applied, true))
            appliedIndex = 0
        }
        if let pendingDraft { revisions.append((pendingDraft, false)) }
    }

    var appliedJSON: String { lock.withLock { appliedIndex.map { revisions[$0].json } ?? "" } }
    var revisionCount: Int { lock.withLock { revisions.count } }
    var latestJSON: String? { lock.withLock { revisions.last?.json } }
    /// Whether orchd would report drift: a newest draft unlike the applied one.
    var drift: Bool {
        lock.withLock {
            guard let last = revisions.indices.last else { return false }
            guard let a = appliedIndex else { return true }
            return last != a && revisions[last].json != revisions[a].json
        }
    }

    /// Another operator apply, as from the terminal or the pane.
    func applyElsewhere(_ json: String) {
        lock.withLock {
            revisions.append((json, true))
            appliedIndex = revisions.count - 1
        }
    }

    func enforcement() async throws -> EnforcementState { EnforcementState(switches: switches) }
    func setEnforcement(layer: String, state: String, forSeconds: Int64) async throws -> EnforcementChangeResult {
        lock.withLock { enforcementCalls.append((layer, state)) }
        return EnforcementChangeResult(current: EnforcementSwitch(layer: layer, state: state))
    }
    func explainPlacement(target: String) async throws -> PlacementExplanation { PlacementExplanation() }
    func spaceLogins() async throws -> [SpaceLogin] { logins }

    func scanSpaces(folders: [String], offline: Bool) async throws -> SpacesScan {
        lock.withLock { scannedFolders.append(folders) }
        if let failScan { throw failScan }
        return scanResult
    }

    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft {
        let rev: Int64 = lock.withLock {
            if let last = revisions.last, last.json == policyJSON, last.byOperator { return Int64(revisions.count) }
            revisions.append((policyJSON, true))
            let rev = Int64(revisions.count)
            if agentDraftAfterSave { revisions.append(("{\"spaces\":[]}", false)) }
            return rev
        }
        return SavedSpacesDraft(revision: rev, problems: problems)
    }

    func diffSpaces() async throws -> SpacesPolicyDiff {
        if let failDiff { throw failDiff }
        return lock.withLock {
            let applied = appliedIndex.map { Int64($0 + 1) } ?? 0
            let latest = Int64(revisions.count)
            let pending = latest > applied
            return SpacesPolicyDiff(appliedRevision: applied, appliedJSON: appliedIndex.map { revisions[$0].json } ?? "",
                                    draftRevision: pending ? latest : 0,
                                    changes: pending ? [SpacesPolicyChange(space: "acme", kind: "added")] : [],
                                    draftCreatedBy: pending ? "a session" : "", draftByOperator: pending ? revisions.last!.byOperator : true)
        }
    }

    func applySpaces(reviewedRevision: Int64) async throws -> Int64 {
        lock.withLock { applyCalls.append(reviewedRevision) }
        if let failApply { throw failApply }
        return try lock.withLock {
            guard reviewedRevision == Int64(revisions.count) else { throw CancellationError() }
            appliedIndex = revisions.count - 1
            return reviewedRevision
        }
    }

    func commandLogRetention() async throws -> CommandLogRetention { retention }
    func setCommandLogRetention(_ value: String) async throws {
        lock.withLock { retentionCalls.append(value) }
    }
}

struct SetupRefused: LocalizedError {
    var errorDescription: String? { "refused" }
}

/// Records whether the device-owner check ran, and can refuse it.
final class Presence: @unchecked Sendable {
    private let lock = NSLock()
    private(set) var asked = 0
    var refuse = false

    func check(_ reason: String) async throws {
        lock.withLock { asked += 1 }
        if refuse { throw SetupRefused() }
    }
}
