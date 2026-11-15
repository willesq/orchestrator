import Foundation

/// One space's outbound terms file as orchd describes it (GetSpaceTerms):
/// never a term, only how many there are and a fingerprint that changes
/// with the file (SP-053, INV-07).
public struct SpaceTermsInfo: Sendable, Equatable {
    public var space: String
    /// The path as the policy writes it; "" when the space names none.
    public var termsFile: String
    public var exists: Bool
    public var count: Int
    /// 12 hex digits of a keyed hash of the file.
    public var fingerprint: String
    /// Why the file cannot be read or parsed; names a line, never a term.
    public var problem: String

    public init(space: String, termsFile: String = "", exists: Bool = false, count: Int = 0, fingerprint: String = "",
                problem: String = "") {
        self.space = space
        self.termsFile = termsFile
        self.exists = exists
        self.count = count
        self.fingerprint = fingerprint
        self.problem = problem
    }

    /// "12 terms · fingerprint 0a1b2c3d4e5f", or why there are none.
    public var summary: String {
        if termsFile.isEmpty { return "No terms file" }
        if !problem.isEmpty { return displaySafe(problem) }
        if !exists { return "No terms yet" }
        let terms = count == 1 ? "1 term" : "\(count) terms"
        return "\(terms) · fingerprint \(displaySafe(fingerprint))"
    }
}

/// A source held in quarantine (ListQuarantine).
public struct QuarantinedSource: Sendable, Equatable {
    public var uri: String
    public var sourceType: String
    public var chunks: Int

    public init(uri: String, sourceType: String = "", chunks: Int = 0) {
        self.uri = uri
        self.sourceType = sourceType
        self.chunks = chunks
    }
}

/// What a retag did (RetagSource).
public struct RetagOutcome: Sendable, Equatable {
    /// The spaces that held the source ("unplaced" for none).
    public var from: [String]
    public var chunks: Int64
    /// false when the source was only stamped where it was.
    public var moved: Bool

    public init(from: [String] = [], chunks: Int64 = 0, moved: Bool = false) {
        self.from = from
        self.chunks = chunks
        self.moved = moved
    }
}

/// Chunks stored per space and how many the applied revision has not
/// labelled yet (Status' SpacesCounts, SP-031; RC-1101).
public struct SpaceChunkCount: Sendable, Equatable {
    public var space: String
    public var chunks: Int
    public var stale: Int

    public init(space: String, chunks: Int, stale: Int = 0) {
        self.space = space
        self.chunks = chunks
        self.stale = stale
    }
}

/// What the advanced editors ask orchd. Every write is the operator's:
/// orchd refuses drafts, terms and retags from anything an agent runs, and
/// applying stays on the Review page (D-71, D-78).
public protocol SpacesEditorAPI: Sendable {
    func status() async throws -> DaemonStatus
    func diffSpaces() async throws -> SpacesPolicyDiff
    func saveSpacesDraft(policyJSON: String) async throws -> SavedSpacesDraft
    func listSessions(includeEnded: Bool) async throws -> [PinnedSession]
    func enforcement() async throws -> EnforcementState
    func explainPlacement(target: String) async throws -> PlacementExplanation
    func listSources(type: String, limit: Int) async throws -> SourceList
    /// Each applied space's terms file, by count and fingerprint only.
    func spaceTerms() async throws -> [SpaceTermsInfo]
    /// Replaces one applied space's terms file; the terms are not kept.
    func setSpaceTerms(space: String, terms: String) async throws -> SpaceTermsInfo
    func quarantinedSources() async throws -> [QuarantinedSource]
    func retagSource(uri: String, space: String, confirm: Bool, expectFrom: String) async throws -> RetagOutcome
    func spaceChunkCounts() async throws -> [SpaceChunkCount]
}
