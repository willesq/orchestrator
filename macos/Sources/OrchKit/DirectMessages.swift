import Foundation
import GRPCCore
import Observation

/// One chat chunk the direct-message search found (SP-083), with the tenant
/// and space orchd stored it under.
public struct DirectMessageHit: Sendable, Equatable, Identifiable {
    public var id: String { "\(sourceURI)#\(index)" }
    /// The hit's place in the results, so two chunks of one thread differ.
    public let index: Int
    public let text: String
    public let sourceURI: String
    /// The chat tenant, "<system>:<tenant>" (e.g. "slack:T0123"); empty when
    /// orchd stored none.
    public let tenant: String
    /// The space orchd placed it in; empty with no spaces policy or with
    /// spaces off.
    public let space: String
    public let score: Float
    /// When the conversation happened, if known.
    public let startedAt: Date?

    public init(index: Int = 0, text: String, sourceURI: String, tenant: String = "", space: String = "", score: Float = 0,
                startedAt: Date? = nil) {
        self.index = index
        self.text = text
        self.sourceURI = sourceURI
        self.tenant = tenant
        self.space = space
        self.score = score
        self.startedAt = startedAt
    }

    /// The tenant as the view labels it.
    public var tenantLabel: String { tenant.isEmpty ? "Unknown tenant" : tenant }
    /// The space as the view labels it: content stored with spaces off or
    /// before a policy names none.
    public var spaceLabel: String { space.isEmpty ? "No space" : space }
}

/// What one direct-message search returned.
public struct DirectMessageResult: Sendable, Equatable {
    public let hits: [DirectMessageHit]
    /// Why a hybrid search answered by meaning only; nil when it ran as asked.
    public let fallback: String?

    public init(hits: [DirectMessageHit], fallback: String? = nil) {
        self.hits = hits
        self.fallback = fallback
    }
}

/// orchd's SearchDirectMessages: memory search with chat direct messages
/// kept in. Operator-only (D-71, D-81): under a policy only Orch.app may
/// call it.
public protocol DirectMessagesAPI: Sendable {
    func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult
}

/// Why a direct-message search showed no results.
public struct DirectMessageSearchFailure: Sendable, Equatable {
    public enum Kind: Sendable, Equatable {
        /// orchd refused the caller: it is not the operator.
        case refused
        /// The request was wrong or orchd could not answer it.
        case failed
    }

    public let kind: Kind
    /// One line saying what happened.
    public let title: String
    /// What the user can do about it, with orchd's own message when it is
    /// safe to show.
    public let detail: String

    public init(kind: Kind, title: String, detail: String) {
        self.kind = kind
        self.title = title
        self.detail = detail
    }

    /// The failure `error` stands for.
    @MainActor static func from(_ error: any Error) -> DirectMessageSearchFailure {
        let rpc = error as? RPCError
        let message = displaySafe(rpc?.message ?? "")
        switch rpc?.code {
        case .permissionDenied?, .unauthenticated?:
            return DirectMessageSearchFailure(
                kind: .refused, title: "orchd refused the search",
                detail: "Only the operator can search direct messages: Orch.app while a spaces policy is applied. "
                    + "Agents, queued tasks and terminals are refused (D-81)."
                    + (message.isEmpty ? "" : " orchd said: \u{201C}\(message)\u{201D}"))
        case .invalidArgument?, .failedPrecondition?:
            return DirectMessageSearchFailure(kind: .failed, title: "The search could not run",
                                              detail: message.isEmpty ? displaySafe(DaemonMonitor.describe(error)) : message)
        case .unimplemented?:
            return DirectMessageSearchFailure(kind: .failed, title: "This orchd cannot search direct messages",
                                              detail: "It needs a newer orchd; update orch (brew upgrade orch).")
        default:
            return DirectMessageSearchFailure(
                kind: .failed, title: "The search failed",
                detail: "orchd answered: \(displaySafe(DaemonMonitor.describe(error))). Check that orchd is running (orch status).")
        }
    }

    /// Shown while screen-share-safe mode is on: no search runs, and
    /// nothing about direct messages shows.
    public static let sharing = DirectMessageSearchFailure(
        kind: .refused, title: "Hidden while screen-share-safe mode is on",
        detail: "Direct messages are never shown while sharing. Turn screen-share-safe mode off in the Spaces pane to search them.")
}

/// Drives Orch.app's direct-message search (SP-083, D-81): runs the
/// operator's SearchDirectMessages and keeps its results, each with its
/// tenant and space.
///
/// While screen-share-safe mode is on it shows no direct message: a search
/// is not sent, results already held are dropped, and `hits` is empty
/// whatever is stored.
@MainActor
@Observable
public final class DirectMessageSearchModel {
    public var query = SearchQuery(text: "", limit: 20, sourceType: chatSourceType)
    public private(set) var isSearching = false
    /// Why the last search showed nothing, or why none may run now.
    public var failure: DirectMessageSearchFailure? {
        isSharing() ? .sharing : stored.failure
    }

    /// The results, empty while sharing.
    public var hits: [DirectMessageHit] { isSharing() ? [] : stored.result?.hits ?? [] }
    /// Why the last search answered by meaning only.
    public var fallback: String? { isSharing() ? nil : stored.result?.fallback }
    /// Whether a search has answered since the last change of query.
    public var hasSearched: Bool { !isSharing() && stored.result != nil }

    /// The source type chat feeds store under (internal/feeds), so the
    /// search returns chat, direct messages included, and not other memory.
    public static let chatSourceType = "chat"

    private let api: any DirectMessagesAPI
    private let isSharing: @MainActor () -> Bool
    private var stored = Stored()
    private var generation = 0

    private struct Stored {
        var result: DirectMessageResult?
        var failure: DirectMessageSearchFailure?
    }

    /// `isSharing` reports screen-share-safe mode (the Spaces pane's).
    public init(api: any DirectMessagesAPI, isSharing: @escaping @MainActor () -> Bool) {
        self.api = api
        self.isSharing = isSharing
    }

    /// Runs the query; does nothing for empty text, and sends nothing while
    /// sharing.
    public func search() async {
        guard !isSharing() else {
            clear()
            return
        }
        let q = query.normalized
        guard !q.text.isEmpty else { return }
        generation += 1
        let mine = generation
        isSearching = true
        defer { if mine == generation { isSearching = false } }
        do {
            let result = try await api.searchDirectMessages(q)
            guard mine == generation else { return }
            // Reason: sharing may have started while the search ran.
            guard !isSharing() else {
                clear()
                return
            }
            stored = Stored(result: result)
        } catch {
            guard mine == generation else { return }
            stored = Stored(failure: DirectMessageSearchFailure.from(error))
        }
    }

    /// Drops every result and failure held, and abandons a search in flight.
    public func clear() {
        generation += 1
        isSearching = false
        stored = Stored()
    }
}
