import Foundation
import Observation

/// What the Review page asks orchd. DiffSpaces and ApplySpaces are
/// operator-only; under a policy only Orch.app counts as the operator
/// (D-71).
public protocol SpacesReviewAPI: Sendable {
    func diffSpaces() async throws -> SpacesPolicyDiff
    /// Saves `policyJSON` as a new draft revision; it changes nothing that
    /// is enforced.
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft
    /// Enforces `reviewedRevision`, which must be the newest draft.
    func applySpaces(reviewedRevision: Int64, acceptAgentDraft: Bool, confirmGitHubOrgs: [String]) async throws -> Int64
    func listSpaces() async throws -> SpacesListing
}

/// SetSpacesDraft's answer.
public struct SavedSpacesDraft: Sendable, Equatable {
    public var revision: Int64
    public var problems: [SpacesProblemInfo]

    public init(revision: Int64, problems: [SpacesProblemInfo] = []) {
        self.revision = revision
        self.problems = problems
    }
}

/// Asks the person at the Mac to prove they are there before Apply: Touch
/// ID or the login password, through the system's sheet. It is a speed bump
/// against an unattended screen; orchd's caller check is the real gate.
public protocol OperatorAuthenticator: Sendable {
    /// Returns once the person confirmed; throws when they cancelled or it
    /// failed.
    func confirm(reason: String) async throws
}

/// Drives the Spaces pane's Review page: the newest draft against the
/// applied revision, its invalid lines, dropping them as a new draft, and
/// Apply of exactly the revision shown, after system authentication.
@MainActor
@Observable
public final class SpacesReviewModel {
    public private(set) var diff: SpacesPolicyDiff?
    public private(set) var review: SpacesReview?
    /// The policy's drift kinds (ListSpaces), e.g. "changed_outside_orchd".
    public private(set) var drift: [String] = []
    public private(set) var lastError: String?
    /// What the last action did, for the user.
    public private(set) var notice: String?
    /// A drop or apply is under way; the actions wait for it.
    public private(set) var isWorking = false
    public private(set) var isLoading = false
    /// The user confirmed they reviewed a draft an agent session saved.
    public var acceptAgentDraft = false
    /// Unconfirmed GitHub orgs whose new identity the user accepts.
    public var confirmOrgs: Set<String> = []

    private let api: any SpacesReviewAPI
    private let authenticator: any OperatorAuthenticator
    private let home: String
    private var generation = 0

    public init(api: any SpacesReviewAPI, authenticator: any OperatorAuthenticator, home: String = NSHomeDirectory()) {
        self.api = api
        self.authenticator = authenticator
        self.home = home
    }

    /// An agent session saved the draft.
    public var needsAgentAcceptance: Bool {
        guard let d = diff, d.draftRevision > 0 else { return false }
        return !d.draftByOperator
    }

    /// Dropping saves the rest of an agent's draft as the user's own
    /// revision, which orchd then records as the operator's (and nothing in
    /// the app could remember otherwise across a relaunch), so the user
    /// accepts the agent's draft first.
    public var canDrop: Bool {
        !isWorking && (review?.excludableCount ?? 0) > 0 && (!needsAgentAcceptance || acceptAgentDraft)
    }

    public var canApply: Bool {
        guard let r = review, r.reviewedRevision > 0, !r.hasProblems, !isWorking else { return false }
        return !needsAgentAcceptance || acceptAgentDraft
    }

    public func load() async {
        generation += 1
        let mine = generation
        isLoading = true
        defer { if mine == generation { isLoading = false } }
        do {
            let d = try await api.diffSpaces()
            let status = try? await api.listSpaces().status
            guard mine == generation else { return }
            if d.draftRevision != diff?.draftRevision {
                acceptAgentDraft = false
                confirmOrgs = []
            }
            diff = d
            review = SpacesReview.build(d, home: home)
            drift = status?.drift ?? []
            lastError = nil
        } catch {
            guard mine == generation else { return }
            lastError = spacesRefusal(error)
        }
    }

    /// Saves the draft without its invalid lines as a new revision, which
    /// the user then reviews and applies. orchd refuses to apply a draft
    /// with any problem, so this is how they are left out.
    public func dropInvalidLines() async {
        guard canDrop, let d = diff, let doc = SpacesReview.excluding(d, home: home) else { return }
        isWorking = true
        var failure: String?
        do {
            let saved = try await api.saveSpacesDraft(policyJSON: doc)
            notice = "Saved rev \(saved.revision) without the invalid lines. Review it, then apply it."
        } catch {
            failure = spacesRefusal(error)
        }
        await finish(failure)
    }

    /// Reads the draft again after an action, keeping the action's error.
    private func finish(_ failure: String?) async {
        isWorking = false
        await load()
        if let failure {
            lastError = failure
            notice = nil
        }
    }

    /// Applies the revision on screen once the system confirms the person
    /// at the Mac. Nothing reaches orchd if they cancel.
    public func apply() async {
        guard canApply, let r = review, let d = diff else { return }
        let revision = r.reviewedRevision
        let accept = needsAgentAcceptance && acceptAgentDraft
        let orgs = d.unconfirmedGitHubOrgs.filter(confirmOrgs.contains)
        isWorking = true
        do {
            try await authenticator.confirm(reason: "apply spaces policy revision \(revision)")
        } catch {
            isWorking = false
            lastError = "Not applied: authentication was cancelled or failed."
            return
        }
        var failure: String?
        do {
            let applied = try await api.applySpaces(reviewedRevision: revision, acceptAgentDraft: accept,
                                                    confirmGitHubOrgs: orgs)
            notice = "Applied rev \(applied)."
        } catch {
            failure = spacesRefusal(error)
        }
        await finish(failure)
    }
}
