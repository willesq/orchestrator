import Foundation
import LocalAuthentication
import Observation

/// Drives the basic setup wizard (SP-052): Scan, Spaces, Recall, Guard,
/// Review. Until Apply nothing that is enforced changes: the scan writes
/// nothing, and the only write before Apply is a draft revision, which
/// enforces nothing. Apply asks for Touch ID or the login password, then
/// applies exactly the draft the user reviewed, then the switches and the
/// retention they chose.
@MainActor
@Observable
public final class SpacesSetupModel {
    public enum Step: Int, Sendable, CaseIterable, Identifiable, Comparable {
        case scan, spaces, recall, enforcement, review

        public var id: Int { rawValue }
        public var title: String {
            switch self {
            case .scan: "Scan"
            case .spaces: "Spaces"
            case .recall: "Recall"
            case .enforcement: "Guard"
            case .review: "Review"
            }
        }

        public static func < (a: Step, b: Step) -> Bool { a.rawValue < b.rawValue }
    }

    /// What Apply did, for the closing screen.
    public struct Outcome: Sendable, Equatable {
        public var done: [String]
        public var failed: [String]
    }

    public private(set) var step: Step = .scan
    public private(set) var folders: [String]
    public private(set) var scan: SpacesScan?
    public private(set) var scanning = false
    public private(set) var scanError: String?
    /// Why setup cannot read the applied policy; setup stops there.
    public private(set) var loadError: String?
    public private(set) var applied = AppliedPolicy()
    public private(set) var groups: [SetupGroup] = []
    /// The kind chosen per group id; a group not here keeps its proposal.
    public private(set) var kinds: [String: SetupKind] = [:]
    /// The gh profile typed per job group id.
    public private(set) var profiles: [String: String] = [:]
    public private(set) var logins: [SpaceLogin] = []
    public private(set) var enforcement: EnforcementState?
    /// The state chosen per layer.
    public private(set) var guardChoices: [String: String] = [:]
    public private(set) var retention: CommandLogRetention?
    public private(set) var retentionChoice = ""
    public private(set) var plan: SetupPlan?
    public private(set) var draft: SavedSpacesDraft?
    public private(set) var reviewChanges: [SpacesPolicyChange] = []
    public private(set) var reviewProblems: [SpacesProblem] = []
    public private(set) var reviewError: String?
    public private(set) var reviewing = false
    public private(set) var applying = false
    public private(set) var applyError: String?
    public private(set) var outcome: Outcome?
    /// The pending draft at the start, which saving setup's draft replaces
    /// as the newest; nil when there was none.
    public private(set) var replacedDraft: (revision: Int64, by: String)?

    public let home: String
    private let api: any SpacesSetupAPI
    private let authorize: @Sendable (String) async throws -> Void
    private var start: SpacesPolicyDiff?
    private var savedJSON: String?

    /// Folders scanned when the user picks none: the usual places for
    /// checkouts that exist under `home`.
    public static let usualFolders = ["code", "work", "src", "Developer", "projects", "repos", "git", "dev"]

    public init(api: any SpacesSetupAPI, home: String = NSHomeDirectory(), folders: [String]? = nil,
                authorize: @escaping @Sendable (String) async throws -> Void = DeviceOwner.confirm) {
        self.api = api
        self.home = home
        self.authorize = authorize
        self.folders = folders ?? Self.usualFolders.map { home + "/" + $0 }.filter {
            var dir: ObjCBool = false
            return FileManager.default.fileExists(atPath: $0, isDirectory: &dir) && dir.boolValue
        }
    }

    /// orchd's refusal, or a local error's own description.
    private func refusal(_ error: any Error) -> String {
        if let local = error as? LocalizedError, let text = local.errorDescription { return displaySafe(text) }
        return spacesRefusal(error)
    }

    // MARK: - Loading and scanning

    /// Reads the applied policy, the logins, the switches and the
    /// retention, then scans.
    public func load() async {
        do {
            let diff = try await api.diffSpaces()
            applied = try AppliedPolicy.parse(diff.appliedJSON)
            start = diff
            replacedDraft = diff.draftRevision > 0 ? (diff.draftRevision, diff.draftCreatedBy) : nil
            loadError = nil
        } catch {
            loadError = refusal(error)
            return
        }
        logins = (try? await api.spaceLogins()) ?? []
        if let state = try? await api.enforcement() {
            enforcement = state
            // Enforcement is on by default (D-11): a first setup starts each
            // layer at its default. A rerun starts at the current state, so
            // a layer the user switched off is not turned back on unasked.
            let first = start?.appliedRevision == 0
            for s in state.switches {
                guardChoices[s.layer] = first ? Self.stricter(s.state, s.defaultState, in: s.states) : s.state
            }
        }
        if let r = try? await api.commandLogRetention() {
            retention = r
            retentionChoice = r.value
        }
        await runScan()
    }

    /// The stricter of two states. Reason: orchd lists a layer's states
    /// default first, not by strength, so setup ranks them itself and never
    /// starts a layer below where the user put it.
    static func stricter(_ a: String, _ b: String, in states: [String]) -> String {
        let rank = ["off": 0, "log": 1, "on": 2, "block-other-jobs": 2, "strict": 3]
        guard !b.isEmpty, states.contains(b), let ra = rank[a], let rb = rank[b] else { return a }
        return rb > ra ? b : a
    }

    public func runScan() async {
        guard !scanning else { return }
        scanning = true
        defer { scanning = false }
        do {
            let found = try await api.scanSpaces(folders: folders, offline: false)
            scan = found
            scanError = nil
            groups = SpacesSetupPlanner.groups(scan: found, applied: applied)
            kinds = kinds.filter { id, _ in groups.contains { $0.id == id } }
            replan()
        } catch {
            scanError = refusal(error)
        }
    }

    public func addFolder(_ path: String) {
        guard path.hasPrefix("/"), !folders.contains(path) else { return }
        folders.append(path)
    }

    public func removeFolder(_ path: String) {
        folders.removeAll { $0 == path }
    }

    // MARK: - Choices

    public func kind(of group: SetupGroup) -> SetupKind { group.locked ? group.proposed : kinds[group.id] ?? group.proposed }

    public func setKind(_ kind: SetupKind, for groupID: String) {
        guard let g = groups.first(where: { $0.id == groupID }), !g.locked else { return }
        kinds[groupID] = kind
        replan()
    }

    public func setProfile(_ dir: String, for groupID: String) {
        profiles[groupID] = dir
        replan()
    }

    public func setGuard(_ state: String, for layer: String) {
        guard let s = enforcement?.switches.first(where: { $0.layer == layer }), s.states.isEmpty || s.states.contains(state)
        else { return }
        guardChoices[layer] = state
    }

    public func setRetention(_ value: String) {
        guard let r = retention, r.options.contains(value) else { return }
        retentionChoice = value
    }

    /// The space a group goes to under the plan; nil when left out.
    public func space(of group: SetupGroup) -> SetupSpace? {
        plan?.spaces.first { $0.orgs.contains { $0.lowercased() == group.id } }
    }

    /// The job spaces this setup creates, each with its group, for the gh
    /// profile list.
    public var newJobs: [(group: SetupGroup, space: SetupSpace)] {
        groups.compactMap { g in
            guard !g.locked, kind(of: g) == .job, let s = space(of: g), s.isNew else { return nil }
            return (g, s)
        }
    }

    /// A job's gh login as orchd last saw it, or how to log in.
    public func loginStatus(for space: SetupSpace) -> SetupLoginStatus {
        SetupLoginStatus.of(space: space.name, dir: space.ghConfigDir, logins: logins, accounts: scan?.accounts ?? [], home: home)
    }

    /// Layers whose chosen state differs from orchd's.
    public var guardChanges: [(layer: EnforcementSwitch, state: String)] {
        (enforcement?.switches ?? []).compactMap { s in
            guard let chosen = guardChoices[s.layer], chosen != s.state else { return nil }
            return (s, chosen)
        }
    }

    public var retentionChange: String? {
        guard let r = retention, !retentionChoice.isEmpty, retentionChoice != r.value else { return nil }
        return retentionChoice
    }

    public var hasChanges: Bool { (plan?.changesPolicy ?? false) || !guardChanges.isEmpty || retentionChange != nil }

    private func replan() {
        guard let scan else { plan = nil; return }
        plan = SpacesSetupPlanner.plan(groups: groups, kinds: kinds, profiles: profiles, scan: scan, applied: applied, home: home)
    }

    // MARK: - Moving between steps

    public var canContinue: Bool {
        switch step {
        case .scan: loadError == nil && scan != nil && !scanning
        case .spaces, .recall, .enforcement: true
        case .review: false
        }
    }

    public func next() async {
        guard canContinue, let following = Step(rawValue: step.rawValue + 1) else { return }
        step = following
        if following == .review { await prepareReview() }
    }

    public func back() {
        guard !applying, let previous = Step(rawValue: step.rawValue - 1) else { return }
        step = previous
    }

    /// Saves the plan as a draft, so orchd validates it and says what it
    /// changes. A draft enforces nothing.
    func prepareReview() async {
        reviewError = nil
        reviewProblems = []
        reviewChanges = []
        guard let plan, plan.changesPolicy else { return }
        if plan.unreadableBase {
            reviewError = "The applied policy could not be read, so setup cannot add to it. Use orch spaces diff to see it."
            return
        }
        reviewing = true
        defer { reviewing = false }
        do {
            if savedJSON != plan.policyJSON || draft == nil {
                draft = try await api.saveSpacesDraft(policyJSON: plan.policyJSON)
                savedJSON = plan.policyJSON
            }
            guard let draft else { return }
            reviewProblems = draft.problems
            let diff = try await api.diffSpaces()
            if diff.draftRevision != draft.revision {
                reviewError = "A newer draft (rev \(diff.draftRevision)) was saved while you reviewed. Go back and continue to review again."
                return
            }
            // Reason: the draft is the applied policy read at the start plus
            // setup's additions; if another revision was applied since,
            // applying it would undo that one.
            if diff.appliedRevision != start?.appliedRevision {
                reviewError = "Another policy revision (\(diff.appliedRevision)) was applied while setup was open. Cancel and run setup again."
                return
            }
            reviewChanges = diff.changes
            if reviewProblems.isEmpty { reviewProblems = diff.problems }
        } catch {
            reviewError = refusal(error)
        }
    }

    public var canApply: Bool {
        step == .review && outcome == nil && !applying && !reviewing && reviewError == nil && reviewProblems.isEmpty
            && hasChanges && (plan?.changesPolicy == false || draft != nil)
    }

    // MARK: - Apply and cancel

    /// Confirms the user is at the Mac, then applies the reviewed draft, the
    /// switches and the retention. If the policy is refused, nothing else
    /// changes.
    public func apply() async {
        guard canApply, let plan else { return }
        applyError = nil
        // Reason: what Apply sends is fixed before the prompt, and the
        // wizard is locked while it shows, so it applies what was on screen.
        applying = true
        defer { applying = false }
        let reviewed = draft?.revision
        let switches = guardChanges
        let keep = retentionChange
        do {
            try await authorize("apply your spaces setup")
        } catch {
            // Reason: a cancelled or failed check says so in the system's own
            // words; anything else reads as not confirmed.
            let why = (error as? LocalizedError)?.errorDescription ?? "you did not confirm it is you."
            applyError = "Not applied: " + displaySafe(why)
            return
        }
        var done: [String] = []
        var failed: [String] = []
        if plan.changesPolicy, let reviewed {
            do {
                let rev = try await api.applySpaces(reviewedRevision: reviewed)
                done.append("Applied policy revision \(rev).")
            } catch {
                applyError = "Not applied: " + refusal(error)
                return
            }
        }
        for (layer, state) in switches {
            do {
                _ = try await api.setEnforcement(layer: layer.layer, state: state, forSeconds: 0)
                done.append("\(displaySafe(layer.name)) set to \(displaySafe(state)).")
            } catch {
                failed.append("\(displaySafe(layer.name)) was not changed: \(refusal(error))")
            }
        }
        if let value = keep {
            do {
                try await api.setCommandLogRetention(value)
                var line = "Command log kept \(CommandLogRetention.label(value))."
                if retention?.needsRestart == true { line += " It takes effect when orchd restarts." }
                done.append(line)
            } catch {
                failed.append("Command log retention was not changed: \(refusal(error))")
            }
        }
        outcome = Outcome(done: done, failed: failed)
    }

    /// Leaves everything as it was. If setup saved a draft over an applied
    /// policy with no other draft pending, it saves the applied policy again
    /// so no drift is left behind. Returns what the user should know.
    @discardableResult
    public func cancel() async -> String? {
        guard outcome == nil, let draft else { return nil }
        // Reason: re-save the applied document only while setup's draft is
        // still the newest and the applied revision is the one setup read,
        // so cancelling never hides a newer draft or revives an old policy.
        if replacedDraft == nil, let start, !start.appliedJSON.isEmpty,
           let now = try? await api.diffSpaces(), now.draftRevision == draft.revision,
           now.appliedRevision == start.appliedRevision {
            if (try? await api.saveSpacesDraft(policyJSON: now.appliedJSON)) != nil {
                self.draft = nil
                return nil
            }
        }
        return "Draft revision \(draft.revision) stays saved. It changes nothing until it is applied."
    }
}

/// A job's gh login, as the Spaces step shows it.
public struct SetupLoginStatus: Sendable, Equatable {
    public enum State: Sendable, Equatable { case loggedIn, notLoggedIn, notChecked }

    public var state: State
    public var text: String
    /// What to run in a terminal to log in; nil when logged in.
    public var command: String?

    static func of(space: String, dir: String, logins: [SpaceLogin], accounts: [ScannedAccount], home: String) -> SetupLoginStatus {
        let want = SpacesSetupPlanner.expand(dir, home: home).lowercased()
        let command = loginCommand(dir)
        if let l = logins.first(where: { $0.space == space && $0.gh.own
            && SpacesSetupPlanner.expand($0.gh.dir, home: home).lowercased() == want }) {
            if l.gh.isLoggedIn {
                return SetupLoginStatus(state: .loggedIn, text: l.gh.account.isEmpty ? "logged in" : "logged in as \(displaySafe(l.gh.account))")
            }
            return SetupLoginStatus(state: .notLoggedIn, text: displaySafe(l.gh.state), command: command)
        }
        if let a = accounts.first(where: { $0.profile == space }) {
            if a.error.isEmpty && !a.login.isEmpty {
                return SetupLoginStatus(state: .loggedIn, text: "logged in as \(displaySafe(a.login))")
            }
            return SetupLoginStatus(state: .notLoggedIn, text: "not logged in", command: command)
        }
        // Reason: orchd checks a profile only once a space names it, so a new
        // job's login is known after Apply; its orgs sync once it is logged in.
        return SetupLoginStatus(state: .notChecked, text: "checked once applied", command: command)
    }

    /// `GH_CONFIG_DIR=… gh auth login` for `dir`, quoted when it needs it.
    /// nil when `dir` holds characters a command cannot show faithfully.
    static func loginCommand(_ dir: String) -> String? {
        guard dir == displaySafe(dir) else { return nil }
        let safe = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-")
        let body = dir.hasPrefix("~/") ? dir.dropFirst(2) : Substring(dir)
        let plain = (dir.hasPrefix("/") || dir.hasPrefix("~/")) && body.unicodeScalars.allSatisfy { safe.contains($0) }
        if plain { return "GH_CONFIG_DIR=\(dir) gh auth login" }
        let quoted = "'" + body.replacingOccurrences(of: "'", with: "'\\''") + "'"
        return "GH_CONFIG_DIR=" + (dir.hasPrefix("~/") ? "\"$HOME\"/" + quoted : quoted) + " gh auth login"
    }
}

/// Asks for Touch ID or the login password before setup applies: a speed
/// bump against an unattended screen. orchd's own check that the caller is
/// the operator is the real gate (INV-06, D-71).
public enum DeviceOwner {
    public static func confirm(_ reason: String) async throws {
        let context = LAContext()
        var error: NSError?
        guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else {
            throw error ?? LAError(.passcodeNotSet)
        }
        _ = try await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)
    }
}
