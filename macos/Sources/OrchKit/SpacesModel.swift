import Foundation
import GRPCCore
import Observation

/// orchd's refusal as the user should read it: an RPC error's own message,
/// without its code; anything else as the other panes show it.
@MainActor
func spacesRefusal(_ error: any Error) -> String {
    if let rpc = error as? RPCError, !rpc.message.isEmpty { return displaySafe(rpc.message) }
    return displaySafe(DocsModel.refusal(error))
}

/// Drives the enforcement switches in Settings and the menu bar panel:
/// reads them, switches a layer (asking first when that stops it
/// enforcing), and keeps them current, so the "enforcement off" banner
/// appears and goes without the user opening anything.
@MainActor
@Observable
public final class EnforcementModel {
    public private(set) var state: EnforcementState?
    public private(set) var lastError: String?
    /// Layers with a switch in flight.
    public private(set) var pending: Set<String> = []
    /// orchd's refusal, per layer, shown under that layer.
    public private(set) var layerErrors: [String: String] = [:]
    /// What else the last switch did, per layer.
    public private(set) var notes: [String: [String]] = [:]
    /// A switch that stops a layer enforcing, waiting for the user to confirm.
    public private(set) var confirmation: EnforcementChange?

    private let api: any SpacesAPI
    private let interval: Duration
    private let now: @Sendable () -> Date
    private var generation = 0
    private var poll: Task<Void, Never>?

    /// `interval` is how often the switches are read while nothing is
    /// timed; a timed switch is read again just after it reverts.
    public init(api: any SpacesAPI, interval: Duration = .seconds(10), now: @escaping @Sendable () -> Date = Date.init) {
        self.api = api
        self.interval = interval
        self.now = now
    }

    /// The layers not enforcing now; empty before the first read.
    public var relaxed: [EnforcementSwitch] { state?.relaxed ?? [] }

    public func load() async {
        generation += 1
        let mine = generation
        do {
            let loaded = try await api.enforcement()
            guard mine == generation else { return }
            state = loaded
            lastError = nil
        } catch {
            guard mine == generation else { return }
            lastError = spacesRefusal(error)
        }
    }

    /// Starts reading the switches; calling it again is a no-op.
    public func start() {
        guard poll == nil else { return }
        poll = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.load()
                try? await Task.sleep(for: self.nextPoll())
            }
        }
    }

    public func stop() {
        poll?.cancel()
        poll = nil
    }

    /// How long until the next read: the interval, or just past the
    /// earliest timer when that comes sooner, so a reverted switch shows
    /// within a second of reverting.
    public func nextPoll() -> Duration {
        guard let expiry = state?.nextExpiry else { return interval }
        let untilExpiry = Duration.milliseconds(Int64(max(0, expiry.timeIntervalSince(now())) * 1000) + 500)
        return min(interval, untilExpiry)
    }

    /// The user picked `state` for a layer. A change that stops the layer
    /// enforcing waits for `confirm()`; anything else is sent at once.
    public func choose(_ layer: EnforcementSwitch, state: String, duration: EnforcementDuration = .untilChanged) async {
        layerErrors[layer.layer] = nil
        guard state != layer.state || duration != .untilChanged else { return }
        let change = EnforcementChange(layer: layer, state: state, duration: duration)
        if layer.relaxes(to: state) {
            confirmation = change
            return
        }
        await send(change)
    }

    public func confirm() async {
        guard let change = confirmation else { return }
        confirmation = nil
        await send(change)
    }

    public func cancel() { confirmation = nil }

    private func send(_ change: EnforcementChange) async {
        let layer = change.layer.layer
        pending.insert(layer)
        defer { pending.remove(layer) }
        do {
            let result = try await api.setEnforcement(layer: layer, state: change.state, forSeconds: change.duration.seconds)
            notes[layer] = result.notes.map(displaySafe)
            if var current = state, let i = current.switches.firstIndex(where: { $0.layer == layer }) {
                current.switches[i] = result.current
                state = current
            }
        } catch {
            layerErrors[layer] = spacesRefusal(error)
        }
        // Another layer can change with this one (the hooks with the guard).
        await load()
    }
}

/// Why a typed path cannot be checked.
public struct PlacementInputError: Error, Equatable, Sendable {
    public let message: String
}

/// The directory validator: type or pick a path and see every root holding
/// it from shallowest to deepest, what its git remote changes, the space it
/// ends up in and why. orchd answers (ExplainPlacement), so the app shows
/// exactly what orchd would do.
@MainActor
@Observable
public final class PlacementValidator {
    public var path = ""
    public private(set) var explanation: PlacementExplanation?
    /// The target the explanation is for.
    public private(set) var checked: String?
    public private(set) var error: String?
    public private(set) var isChecking = false

    private let api: any SpacesAPI
    private let home: String
    private var generation = 0

    public init(api: any SpacesAPI, home: String = NSHomeDirectory()) {
        self.api = api
        self.home = home
    }

    /// The absolute path to ask about: `~` expands to the home folder,
    /// surrounding spaces are dropped, and anything relative is refused.
    public static func target(_ typed: String, home: String) -> Result<String, PlacementInputError> {
        var path = typed.trimmingCharacters(in: .whitespacesAndNewlines)
        if path == "~" {
            path = home
        } else if path.hasPrefix("~/") {
            path = home + path.dropFirst(1)
        }
        guard !path.isEmpty else { return .failure(PlacementInputError(message: "Type or choose a folder.")) }
        guard path.hasPrefix("/") else {
            return .failure(PlacementInputError(message: "Use a full path, starting with / or ~."))
        }
        return .success(path)
    }

    /// The roots, shallowest first, each marked when it is the one that decides.
    public var chain: [(root: PlacementRoot, decides: Bool)] {
        guard let roots = explanation?.roots else { return [] }
        return roots.enumerated().map { ($0.element, $0.offset == roots.count - 1) }
    }

    /// Checks a path chosen in the open panel exactly as given: a folder
    /// name may end in a space, so it is not trimmed like a typed one.
    public func check(picked: String) async {
        path = picked
        guard picked.hasPrefix("/") else { return await check() }
        await ask(picked)
    }

    public func check() async {
        let target: String
        switch Self.target(path, home: home) {
        case .failure(let e):
            error = e.message
            explanation = nil
            checked = nil
            return
        case .success(let t):
            target = t
        }
        await ask(target)
    }

    private func ask(_ target: String) async {
        generation += 1
        let mine = generation
        isChecking = true
        defer { if mine == generation { isChecking = false } }
        do {
            let answer = try await api.explainPlacement(target: target)
            guard mine == generation else { return }
            explanation = answer
            checked = target
            error = nil
        } catch {
            guard mine == generation else { return }
            explanation = nil
            checked = nil
            self.error = spacesRefusal(error)
        }
    }
}
