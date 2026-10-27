import Foundation
import Observation

/// The orchd calls behind the Safety Net tab.
public protocol SafetyNetAPI: Sendable {
    /// How cc-safety-net is installed against the pinned version, the guard
    /// rules, and the last install action.
    func safetyNet() async throws -> SafetyNetState
    /// Installs, upgrades or repairs cc-safety-net; `rulesOnly` rewrites only
    /// the orch-guards rules. orchd runs one at a time and refuses the rest.
    func installSafetyNet(rulesOnly: Bool) async throws -> SafetyNetState
}

/// cc-safety-net as orchd sees it in Claude Code's plugin files.
public struct SafetyNetState: Sendable, Equatable {
    /// missing, legacy, outdated, disabled or ok.
    public let state: String
    /// The plugin is at the pin and enabled, and the rules are in place.
    public let ok: Bool
    public let pinnedVersion: String
    public let pinnedSHA: String
    public let pinnedReleasedOn: String
    /// Empty when orch's copy is not installed.
    public let installedVersion: String
    public let installedSHA: String
    /// Competing copies from other marketplaces.
    public let legacy: [SafetyNetInstall]
    public let rules: SafetyNetRules
    /// Plugin files that could not be read.
    public let errors: [String]
    public let action: SafetyNetAction

    public init(state: String = "", ok: Bool = false, pinnedVersion: String = "", pinnedSHA: String = "",
                pinnedReleasedOn: String = "", installedVersion: String = "", installedSHA: String = "",
                legacy: [SafetyNetInstall] = [], rules: SafetyNetRules = SafetyNetRules(), errors: [String] = [],
                action: SafetyNetAction = SafetyNetAction()) {
        self.state = state
        self.ok = ok
        self.pinnedVersion = pinnedVersion
        self.pinnedSHA = pinnedSHA
        self.pinnedReleasedOn = pinnedReleasedOn
        self.installedVersion = installedVersion
        self.installedSHA = installedSHA
        self.legacy = legacy
        self.rules = rules
        self.errors = errors
        self.action = action
    }

    /// The badge shown for the plugin.
    public var badge: (label: String, tone: Tone) {
        switch state {
        case "ok": rules.ok ? ("Protected", .good) : ("Rules incomplete", .warning)
        case "missing": ("Not installed", .bad)
        case "legacy": ("Old copy installed", .warning)
        case "outdated": ("Update available", .warning)
        case "disabled": ("Disabled", .warning)
        default: ("Unknown", .neutral)
        }
    }

    public enum Tone: Sendable { case good, warning, bad, neutral }

    /// Install applies: orch's copy is missing, disabled or competing with another.
    public var canInstall: Bool { ["missing", "legacy", "disabled"].contains(state) }
    /// Upgrade applies: orch's copy is at another version or commit.
    public var canUpgrade: Bool { state == "outdated" }
    /// Repair applies: the guard rules are missing, stale or not enabled.
    public var canRepairRules: Bool { !rules.ok }

    /// The one action that applies now, most fundamental first: a missing or
    /// competing plugin is installed before it can be upgraded, and the rules
    /// are repaired once the plugin itself is right. nil when up to date.
    public var nextAction: SafetyNetNextAction? {
        if canInstall { return .install }
        if canUpgrade { return .upgrade }
        if canRepairRules { return .repairRules }
        return nil
    }
}

/// An action the Safety Net tab offers.
public enum SafetyNetNextAction: Sendable, Equatable {
    case install, upgrade, repairRules

    public var title: String {
        switch self {
        case .install: "Install"
        case .upgrade: "Upgrade"
        case .repairRules: "Repair rules"
        }
    }

    public var help: String {
        switch self {
        case .install: "Install the pinned cc-safety-net into Claude Code, replacing any other copy"
        case .upgrade: "Move cc-safety-net to the version this orch pins"
        case .repairRules: "Rewrite the orch-guards rulebook and enable it, keeping your own rulebooks"
        }
    }
}

public struct SafetyNetInstall: Sendable, Equatable, Identifiable {
    public var id: String { "\(pluginID)/\(scope)" }
    public let pluginID: String
    public let scope: String
    public let version: String

    public init(pluginID: String, scope: String = "user", version: String = "") {
        self.pluginID = pluginID
        self.scope = scope
        self.version = version
    }
}

/// The user-scope cc-safety-net rules.
public struct SafetyNetRules: Sendable, Equatable {
    public let ok: Bool
    public let configPath: String
    public let configExists: Bool
    public let configError: String
    public let referenced: Bool
    public let rtkWrapper: Bool
    public let rulebookCurrent: Bool
    public let rulebooks: [SafetyNetRulebook]
    /// What Repair rules would fix.
    public let problems: [String]

    public init(ok: Bool = false, configPath: String = "", configExists: Bool = false, configError: String = "",
                referenced: Bool = false, rtkWrapper: Bool = false, rulebookCurrent: Bool = false,
                rulebooks: [SafetyNetRulebook] = [], problems: [String] = []) {
        self.ok = ok
        self.configPath = configPath
        self.configExists = configExists
        self.configError = configError
        self.referenced = referenced
        self.rtkWrapper = rtkWrapper
        self.rulebookCurrent = rulebookCurrent
        self.rulebooks = rulebooks
        self.problems = problems
    }
}

/// One rulebook source listed in rule.json: orch-guards, or one the user owns.
public struct SafetyNetRulebook: Sendable, Equatable, Identifiable {
    public var id: String { source }
    public let source: String
    public let name: String
    /// "orch" or "user".
    public let owner: String
    public let remote: Bool
    public let version: String
    public let description: String
    /// Set when the rulebook is missing or invalid, so its rules do not apply.
    public let error: String
    public let rules: [SafetyNetRule]

    public init(source: String, name: String = "", owner: String = "user", remote: Bool = false, version: String = "",
                description: String = "", error: String = "", rules: [SafetyNetRule] = []) {
        self.source = source
        self.name = name.isEmpty ? source : name
        self.owner = owner
        self.remote = remote
        self.version = version
        self.description = description
        self.error = error
        self.rules = rules
    }

    public var isOrch: Bool { owner == "orch" }

    /// The settings item for one of its rules, as the cc-safety-net settings
    /// provider names it ("<rulebook>/<rule>"), so this tab and the Claude
    /// tab toggle the same thing.
    public func settingRef(for rule: SafetyNetRule) -> SettingRef {
        SettingRef(provider: SafetyNetRulebook.settingsProvider, kind: "rule", identity: "\(name)/\(rule.name)")
    }

    /// The settings provider for cc-safety-net's rules.
    public static let settingsProvider = "cc-safety-net"
}

public struct SafetyNetRule: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let command: String
    public let subcommand: String
    public let blockArgs: [String]
    public let reason: String
    /// Turned off by an override in rule.json.
    public let off: Bool

    public init(name: String, command: String = "", subcommand: String = "", blockArgs: [String] = [], reason: String = "",
                off: Bool = false) {
        self.name = name
        self.command = command
        self.subcommand = subcommand
        self.blockArgs = blockArgs
        self.reason = reason
        self.off = off
    }

    /// "git push" for a rule on git's push subcommand.
    public var commandLine: String { subcommand.isEmpty ? command : "\(command) \(subcommand)" }

    /// What the rule blocks, as one phrase: "git worktree add", or
    /// "git checkout -b, -B or --orphan" when it blocks several arguments,
    /// or "any git push" when it blocks the command outright.
    public var blockedPhrase: String {
        switch blockArgs.count {
        case 0: "any \(commandLine)"
        case 1: "\(commandLine) \(blockArgs[0])"
        default: "\(commandLine) \(blockArgs.dropLast().joined(separator: ", ")) or \(blockArgs.last!)"
        }
    }
}

/// The running install action, or the last one.
public struct SafetyNetAction: Sendable, Equatable {
    public let running: Bool
    public let rulesOnly: Bool
    public let startedAt: Date?
    public let finishedAt: Date?
    /// The step running, or the last one.
    public let step: String
    public let ok: Bool
    public let error: String
    /// Command output, capped by orchd.
    public let log: String

    public init(running: Bool = false, rulesOnly: Bool = false, startedAt: Date? = nil, finishedAt: Date? = nil,
                step: String = "", ok: Bool = false, error: String = "", log: String = "") {
        self.running = running
        self.rulesOnly = rulesOnly
        self.startedAt = startedAt
        self.finishedAt = finishedAt
        self.step = step
        self.ok = ok
        self.error = error
        self.log = log
    }

    /// Whether an action has ever run in this orchd.
    public var hasRun: Bool { startedAt != nil }

    /// The log made safe to show line by line: each line goes through
    /// displaySafe, so control characters inside a line become escapes while
    /// the line breaks themselves are kept.
    public var displayLog: String {
        log.split(separator: "\n", omittingEmptySubsequences: false)
            .map { displaySafe(String($0)) }
            .joined(separator: "\n")
    }
}

extension DaemonClient: SafetyNetAPI {
    public func safetyNet() async throws -> SafetyNetState {
        Self.safetyNet(try await getSafetyNet())
    }

    public func installSafetyNet(rulesOnly: Bool) async throws -> SafetyNetState {
        Self.safetyNet(try await runSafetyNetInstall(rulesOnly: rulesOnly))
    }

    static func safetyNet(_ s: Orchd_V1_SafetyNetState) -> SafetyNetState {
        let r = s.rules
        let a = s.action
        return SafetyNetState(
            state: s.state, ok: s.ok, pinnedVersion: s.pinnedVersion, pinnedSHA: s.pinnedSha,
            pinnedReleasedOn: s.pinnedReleasedOn, installedVersion: s.installedVersion, installedSHA: s.installedSha,
            legacy: s.legacy.map { SafetyNetInstall(pluginID: $0.id, scope: $0.scope, version: $0.version) },
            rules: SafetyNetRules(
                ok: r.ok, configPath: r.configPath, configExists: r.configExists, configError: r.configError,
                referenced: r.referenced, rtkWrapper: r.rtkWrapper, rulebookCurrent: r.rulebookCurrent,
                rulebooks: r.rulebooks.map { rb in
                    SafetyNetRulebook(
                        source: rb.source, name: rb.name, owner: rb.owner, remote: rb.remote, version: rb.version,
                        description: rb.description_p, error: rb.error,
                        rules: rb.rules.map {
                            SafetyNetRule(name: $0.name, command: $0.command, subcommand: $0.subcommand,
                                          blockArgs: $0.blockArgs, reason: $0.reason, off: $0.off)
                        })
                },
                problems: r.problems),
            errors: s.errors,
            action: SafetyNetAction(
                running: a.running, rulesOnly: a.rulesOnly, startedAt: date(fromUnix: a.startedAt),
                finishedAt: date(fromUnix: a.finishedAt), step: a.step, ok: a.ok, error: a.error, log: a.log)
        )
    }
}

/// Drives the Safety Net tab: the plugin and rules state, and the Install,
/// Upgrade and Repair rules actions with their progress and result.
@MainActor
@Observable
public final class SafetyNetModel {
    public private(set) var state: SafetyNetState?
    public private(set) var lastError: String?
    /// What the last action did, shown under the actions.
    public private(set) var message: String?
    /// An action started here is running.
    public private(set) var busy = false

    private let api: any SafetyNetAPI

    public init(api: any SafetyNetAPI) {
        self.api = api
    }

    public func load() async {
        do {
            state = try await api.safetyNet()
            lastError = nil
        } catch {
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Installs or migrates the plugin and its rules.
    public func install() async { await run(rulesOnly: false, verb: "Installed") }

    /// Upgrades the plugin to the pinned version.
    public func upgrade() async { await run(rulesOnly: false, verb: "Upgraded") }

    /// Rewrites the orch-guards rulebook and its rule.json entry only.
    public func repairRules() async { await run(rulesOnly: true, verb: "Repaired the rules") }

    /// Runs the action the tab offers.
    public func perform(_ action: SafetyNetNextAction) async {
        switch action {
        case .install: await install()
        case .upgrade: await upgrade()
        case .repairRules: await repairRules()
        }
    }

    /// Runs one action, polling orchd for its current step meanwhile. orchd
    /// runs one action at a time; a refusal is shown, not queued.
    func run(rulesOnly: Bool, verb: String, poll: Duration = .seconds(1)) async {
        guard !busy else { return }
        busy = true
        lastError = nil
        message = nil
        defer { busy = false }
        let api = self.api
        let follower = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: poll)
                guard !Task.isCancelled, let fresh = try? await api.safetyNet() else { continue }
                // Checked again after the call: a poll that returns once the
                // action has finished must not replace its final state.
                guard !Task.isCancelled, fresh.action.running else { continue }
                self?.state = fresh
            }
        }
        let outcome: Result<SafetyNetState, any Error>
        do {
            outcome = .success(try await api.installSafetyNet(rulesOnly: rulesOnly))
        } catch {
            outcome = .failure(error)
        }
        // Both run on the main actor, so the follower sees this before it can
        // write again.
        follower.cancel()
        switch outcome {
        case .success(let result):
            state = result
            if result.action.ok {
                message = rulesOnly
                    ? "\(verb). Claude Code reads them on its next command."
                    : "\(verb). Restart running Claude Code sessions to load the plugin."
            } else {
                lastError = result.action.error.isEmpty ? "The install did not finish." : result.action.error
            }
        case .failure(let error):
            lastError = DocsModel.refusal(error)
        }
    }
}
