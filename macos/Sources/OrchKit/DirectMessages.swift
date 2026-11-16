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
    /// Every field orchd sent, for the inspector; built from the fields
    /// above when orchd's full hit is not at hand.
    public let detail: SearchHit

    public init(index: Int = 0, text: String, sourceURI: String, tenant: String = "", space: String = "", score: Float = 0,
                startedAt: Date? = nil, detail: SearchHit? = nil) {
        self.index = index
        self.text = text
        self.sourceURI = sourceURI
        self.tenant = tenant
        self.space = space
        self.score = score
        self.startedAt = startedAt
        self.detail = detail ?? SearchHit(text: text, sourceURI: sourceURI, score: score, startedAt: startedAt,
                                          space: space, tenant: tenant)
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
/// Like the Memory search it reads `key:value` filters out of the text,
/// fetches one ranked batch of `SearchQuery.maxResults` and reveals it
/// `query.limit` hits at a time above a relevance floor.
///
/// While screen-share-safe mode is on it shows no direct message: a search
/// is not sent, results already held are dropped, and `hits` is empty
/// whatever is stored.
@MainActor
@Observable
public final class DirectMessageSearchModel {
    /// The menus' filters, the text with any typed filters, and the page
    /// size; the Type menu starts at chat.
    public var query = SearchQuery(text: "", limit: 20, sourceType: chatSourceType)
    public private(set) var isSearching = false
    /// The filters, chips and notes read from the last search's text.
    public private(set) var parsed = ParsedMemoryQuery()
    /// The query the results answer, as sent.
    public private(set) var answered: SearchQuery?
    /// Hits scoring below this are hidden from the batch; 0 hides none.
    public var minRelevance: Float = 0
    /// Why the last search showed nothing, or why none may run now.
    public var failure: DirectMessageSearchFailure? {
        isSharing() ? .sharing : stored.failure
    }

    /// The whole batch, empty while sharing.
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
    private var pages = 1

    private struct Stored {
        var result: DirectMessageResult?
        var failure: DirectMessageSearchFailure?
    }

    /// `isSharing` reports screen-share-safe mode (the Spaces pane's).
    public init(api: any DirectMessagesAPI, isSharing: @escaping @MainActor () -> Bool) {
        self.api = api
        self.isSharing = isSharing
    }

    /// Runs the query with its typed filters; does nothing without words
    /// to search for, and sends nothing while sharing.
    public func search() async {
        guard !isSharing() else {
            clear()
            return
        }
        parsed = MemoryFilterParser.parse(query.text)
        var q = parsed.applied(to: query)
        q.limit = SearchQuery.maxResults
        q = q.normalized
        guard !q.text.isEmpty else {
            if parsed.hasFilters { parsed.notes.append("Add words to search for.") }
            return
        }
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
            answered = q
            pages = 1
        } catch {
            guard mine == generation else { return }
            stored = Stored(failure: DirectMessageSearchFailure.from(error))
            answered = q
        }
    }

    /// Drops one typed filter from the text and searches again.
    public func remove(_ chip: MemoryFilterChip) async {
        query.text = MemoryFilterParser.removing(chip, from: query.text)
        await search()
    }

    /// Narrows the search to one conversation, replacing any `uri:` filter,
    /// and runs it when there are words to search for.
    public func within(source uri: String) async {
        var text = query.text
        for chip in MemoryFilterParser.parse(text).chips.reversed() where chip.key == .uri {
            text = MemoryFilterParser.removing(chip, from: text)
        }
        guard let token = MemoryFilterParser.token(.uri, uri) else { return }
        query.text = text.isEmpty ? token : "\(text) \(token)"
        if !MemoryFilterParser.parse(query.text).text.isEmpty { await search() }
    }

    /// Drops every result and failure held, and abandons a search in flight.
    public func clear() {
        generation += 1
        isSearching = false
        stored = Stored()
        answered = nil
    }

    // MARK: Pages of the batch

    /// Hits shown per page.
    public var pageSize: Int { min(max(query.limit, 1), SearchQuery.maxResults) }

    /// The batch's hits at or above `minRelevance`.
    public var matchingHits: [DirectMessageHit] { hits.filter { $0.score >= minRelevance } }

    /// The hits revealed so far.
    public var visibleHits: [DirectMessageHit] { Array(matchingHits.prefix(pages * pageSize)) }

    public var canShowMore: Bool { visibleHits.count < matchingHits.count }

    /// Reveals the next page of the batch, without asking orchd again.
    public func showMore() {
        if canShowMore { pages += 1 }
    }

    /// "Showing 20 of 100", as the Memory search says it.
    public var footer: String? {
        guard !hits.isEmpty else { return nil }
        return rankedBatchFooter(visible: visibleHits.count, matching: matchingHits.count, batch: hits.count,
                                 minRelevance: minRelevance)
    }
}
