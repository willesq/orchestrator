import Foundation
import Observation

/// Drives the Spaces pane's Overview and the sidebar badge: reads the
/// applied policy, the sessions and the command log summary from orchd,
/// tells conflicts from other quarantine, and keeps it current.
///
/// In screen-share-safe mode it keeps only the chosen space's data: what
/// orchd returns about other spaces is dropped before anything is stored,
/// so no view built from this model can show it.
@MainActor
@Observable
public final class SpacesOverviewModel {
    public private(set) var overview: SpacesOverview?
    public private(set) var lastError: String?
    public private(set) var isLoading = false

    /// Basic or Advanced; kept across launches.
    public var detail: SpacesDetail {
        didSet { defaults.set(detail.rawValue, forKey: Self.detailKey) }
    }

    /// The one space screen-share-safe mode shows; nil when every space
    /// shows. Kept across launches, so a relaunch during a share stays safe.
    public private(set) var shareSafe: String?

    /// Every space, for choosing the one to share (id: orchd's name, name:
    /// safe to show); empty in screen-share-safe mode, which must not list
    /// the others.
    public var spaceChoices: [(id: String, name: String)] {
        shareSafe == nil ? overview?.cards.map { ($0.id, $0.name) } ?? [] : []
    }

    /// Whether the app may post notifications: none while sharing, since
    /// they name every space's PRs.
    public var allowsNotifications: Bool { shareSafe == nil }

    /// Shown when a read fails while sharing: orchd's own message could
    /// name another space.
    public static let sharingError = "Spaces could not be read. Run orch spaces status in a terminal for details."

    /// The sidebar badge; none until the first read.
    public var badge: SpacesBadge { overview?.badge ?? .none }

    public static let detailKey = "spacesDetail"
    public static let shareSafeKey = "spacesShareSafe"

    private let api: any SpacesOverviewAPI
    private let defaults: UserDefaults
    private let home: String
    private let interval: Duration
    private let relabel: @Sendable () async -> RelabelProgress?
    private var generation = 0
    private var poll: Task<Void, Never>?

    /// `relabel` reports a running relabel (SP-027); orchd has no RPC for it
    /// yet, so by default there is none.
    public init(api: any SpacesOverviewAPI, defaults: UserDefaults = .standard, home: String = NSHomeDirectory(),
                interval: Duration = .seconds(60), relabel: @escaping @Sendable () async -> RelabelProgress? = { nil }) {
        self.api = api
        self.defaults = defaults
        self.home = home
        self.interval = interval
        self.relabel = relabel
        detail = defaults.string(forKey: Self.detailKey).flatMap(SpacesDetail.init(rawValue:)) ?? .basic
        shareSafe = defaults.string(forKey: Self.shareSafeKey).flatMap { $0.isEmpty ? nil : $0 }
    }

    /// Shows only `space` until `showAll()`. What is already loaded is cut
    /// down at once, before the next read.
    public func shareOnly(_ space: String) async {
        shareSafe = space
        defaults.set(space, forKey: Self.shareSafeKey)
        generation += 1
        overview = overview?.restricted(to: space)
        if lastError != nil { lastError = Self.sharingError }
        await load()
    }

    public func showAll() async {
        shareSafe = nil
        defaults.removeObject(forKey: Self.shareSafeKey)
        await load()
    }

    public func setDetail(_ detail: SpacesDetail) async {
        guard detail != self.detail else { return }
        self.detail = detail
        await load()
    }

    public func load() async {
        generation += 1
        let mine = generation
        isLoading = true
        defer { if mine == generation { isLoading = false } }
        let listing: SpacesListing
        do {
            listing = try await api.listSpaces()
        } catch {
            guard mine == generation else { return }
            lastError = shareSafe == nil ? spacesRefusal(error) : Self.sharingError
            // A failed read must not leave other spaces' data behind once
            // only one may show.
            if let safe = shareSafe { overview = overview?.restricted(to: safe) }
            return
        }
        var built: SpacesOverview
        if listing.status.configured {
            built = await overview(listing)
        } else {
            built = SpacesOverview.build(listing: listing, sessions: [], activity: [], conflicted: [], unconfirmedOrgs: [],
                                         relabel: nil, detail: detail, home: home)
        }
        guard mine == generation else { return }
        if let safe = shareSafe { built = built.restricted(to: safe) }
        overview = built
        lastError = nil
    }

    /// Starts reading every `interval`, so the badge stays current with the
    /// pane closed; calling it again is a no-op.
    public func start() {
        guard poll == nil else { return }
        poll = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.load()
                try? await Task.sleep(for: self.interval)
            }
        }
    }

    public func stop() {
        poll?.cancel()
        poll = nil
    }

    private func overview(_ listing: SpacesListing) async -> SpacesOverview {
        var problems: [String] = []
        async let sessionsRead = api.listSessions(includeEnded: detail == .advanced)
        async let activityRead = api.toolLogSummary()
        async let orgsRead = api.unconfirmedGitHubOrgs()
        async let relabelRead = relabel()
        var sessions: [PinnedSession] = []
        var activity: [SpaceActivity] = []
        var orgs: [String] = []
        do { sessions = try await sessionsRead } catch { problems.append("Sessions: " + spacesRefusal(error)) }
        do { activity = try await activityRead } catch { problems.append("Command log: " + spacesRefusal(error)) }
        do { orgs = try await orgsRead } catch { problems.append("GitHub orgs: " + spacesRefusal(error)) }
        let progress = await relabelRead
        // In screen-share-safe mode conflicts and quarantine are not shown,
        // so their folders are not even asked about.
        let conflicted = shareSafe == nil ? await conflicts(among: sessions) : []
        return SpacesOverview.build(listing: listing, sessions: sessions, activity: activity, conflicted: conflicted,
                                    unconfirmedOrgs: orgs, relabel: progress, detail: detail, home: home,
                                    problems: problems)
    }

    /// The quarantined live sessions whose folder orchd places in a conflict
    /// now; a session pinned to quarantine is otherwise just unplaced.
    private func conflicts(among sessions: [PinnedSession]) async -> Set<String> {
        let quarantined = sessions.filter { $0.isLive && $0.space == "quarantine" && !$0.firstCwd.isEmpty }
        var found: Set<String> = []
        for session in quarantined.prefix(SpacesOverview.conflictChecks) {
            if let answer = try? await api.explainPlacement(target: session.firstCwd), answer.outcome == "conflict" {
                found.insert(session.sessionID)
            }
        }
        return found
    }
}
