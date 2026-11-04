import Foundation
import Observation

/// How a setting's value is shaped, and so how it is edited.
public enum ConfigFieldType: String, Sendable, Equatable {
    case bool, int, float, string, duration, `enum`, path, url, list, map
}

/// One setting from config.yaml, as orchd describes it. Values are JSON, as
/// orchd sends and accepts them.
public struct ConfigField: Sendable, Equatable, Identifiable {
    public var id: String { key }
    public var key: String
    public var section: String
    public var label: String
    public var help: String
    public var type: ConfigFieldType
    public var value: String
    public var defaultValue: String
    public var options: [String]
    public var range: ClosedRange<Double>?
    public var sensitive: Bool
    public var live: Bool
    public var envOverride: Bool
    public var pendingRestart: Bool

    public init(key: String, section: String = "", label: String = "", help: String = "", type: ConfigFieldType = .string,
                value: String = "\"\"", defaultValue: String = "\"\"", options: [String] = [], range: ClosedRange<Double>? = nil,
                sensitive: Bool = false, live: Bool = false, envOverride: Bool = false, pendingRestart: Bool = false) {
        self.key = key
        self.section = section
        self.label = label
        self.help = help
        self.type = type
        self.value = value
        self.defaultValue = defaultValue
        self.options = options
        self.range = range
        self.sensitive = sensitive
        self.live = live
        self.envOverride = envOverride
        self.pendingRestart = pendingRestart
    }

    public var isDefault: Bool { ConfigJSON.same(value, defaultValue) }
    public var boolValue: Bool { ConfigJSON.decode(value) as? Bool ?? false }
    public var intValue: Int { (ConfigJSON.decode(value) as? NSNumber)?.intValue ?? 0 }
    /// The value as one editable line of text.
    public var text: String { ConfigJSON.text(value) }
    /// A list as one item per line, a map as indented JSON.
    public var multilineText: String {
        if type == .list { return ConfigJSON.list(value).joined(separator: "\n") }
        return ConfigJSON.pretty(value)
    }

    /// The JSON to send for what was typed, or why it cannot be sent. orchd
    /// validates fully; this catches what the form itself can tell.
    public func json(fromText typed: String) -> Result<String, ConfigInputError> {
        let t = typed.trimmingCharacters(in: .whitespacesAndNewlines)
        switch type {
        case .bool:
            return .success(t == "true" ? "true" : "false")
        case .int:
            guard let n = Int(t) else { return .failure(.init("\(label) must be a whole number.")) }
            if let range, !range.contains(Double(n)) {
                return .failure(.init("\(label) must be from \(Int(range.lowerBound)) to \(Int(range.upperBound))."))
            }
            return .success(String(n))
        case .float:
            guard let x = Double(t), x.isFinite else { return .failure(.init("\(label) must be a number.")) }
            if let range, !range.contains(x) {
                return .failure(.init("\(label) must be from \(range.lowerBound) to \(range.upperBound)."))
            }
            return .success(ConfigJSON.encode(x))
        case .list:
            let items = typed.split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
            return .success(ConfigJSON.encode(items))
        case .map:
            guard let data = t.data(using: .utf8),
                  (try? JSONSerialization.jsonObject(with: data)) is [String: Any] else {
                return .failure(.init("\(label) must be a JSON object."))
            }
            return .success(t)
        default:
            return .success(ConfigJSON.encode(t))
        }
    }
}

public struct ConfigInputError: Error, Equatable, Sendable {
    public let message: String
    public init(_ message: String) { self.message = message }
}

/// Reading and writing the JSON values settings travel as.
public enum ConfigJSON {
    static func decode(_ json: String) -> Any? {
        guard let data = json.data(using: .utf8) else { return nil }
        return try? JSONSerialization.jsonObject(with: data, options: .fragmentsAllowed)
    }

    static func encode(_ value: Any) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: value, options: [.fragmentsAllowed, .sortedKeys, .withoutEscapingSlashes]),
              let s = String(data: data, encoding: .utf8) else { return "null" }
        return s
    }

    static func same(_ a: String, _ b: String) -> Bool {
        guard let x = decode(a), let y = decode(b) else { return a == b }
        return encode(x) == encode(y)
    }

    static func text(_ json: String) -> String {
        switch decode(json) {
        case let s as String: s
        case let n as NSNumber: n.stringValue
        default: json
        }
    }

    static func list(_ json: String) -> [String] {
        decode(json) as? [String] ?? []
    }

    public static func pretty(_ json: String) -> String {
        guard let value = decode(json),
              let data = try? JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys, .fragmentsAllowed, .withoutEscapingSlashes]),
              let s = String(data: data, encoding: .utf8) else { return json }
        return s
    }
}

/// Every setting in config.yaml, and whether orchd needs a restart for them.
public struct ConfigState: Sendable, Equatable {
    public var fields: [ConfigField]
    public var path: String
    public var restartRequired: Bool

    public init(fields: [ConfigField] = [], path: String = "", restartRequired: Bool = false) {
        self.fields = fields
        self.path = path
        self.restartRequired = restartRequired
    }
}

public protocol ConfigAPI: Sendable {
    func config() async throws -> ConfigState
    func setConfig(_ values: [String: String], confirmSensitive: Bool) async throws -> ConfigState
}

/// A change to a sensitive setting, waiting for the user to confirm it.
public struct ConfigChange: Sendable, Equatable, Identifiable {
    public var id: String { field.key }
    public let field: ConfigField
    public let json: String
}

/// Drives the Settings pane's view of config.yaml. A change is written at once;
/// a failure stays with that field, so a view shows it inline.
@MainActor
@Observable
public final class ConfigModel {
    public private(set) var state: ConfigState?
    public private(set) var lastError: String?
    public private(set) var pending: Set<String> = []
    public private(set) var fieldErrors: [String: String] = [:]
    /// A sensitive change waiting for confirmation.
    public var confirmation: ConfigChange?
    public private(set) var isRestarting = false
    public private(set) var restartError: String?

    private let api: any ConfigAPI
    private let restartDaemon: @Sendable () async throws -> Void
    private let settleDelay: Duration
    /// Bumped per load, so an older reply cannot replace a newer one.
    private var generation = 0

    public init(api: any ConfigAPI, settleDelay: Duration = .seconds(1), restartDaemon: @escaping @Sendable () async throws -> Void) {
        self.api = api
        self.settleDelay = settleDelay
        self.restartDaemon = restartDaemon
    }

    /// How many settings wait for a restart.
    public var pendingRestartCount: Int { state?.fields.filter(\.pendingRestart).count ?? 0 }

    public func fields(section: String) -> [ConfigField] {
        state?.fields.filter { $0.section == section } ?? []
    }

    public func load() async {
        generation += 1
        let mine = generation
        do {
            let loaded = try await api.config()
            guard mine == generation else { return }
            state = loaded
            lastError = nil
        } catch {
            guard mine == generation else { return }
            lastError = displaySafe(DocsModel.refusal(error))
        }
    }

    /// Writes a new value, or asks first when the setting is sensitive.
    public func change(_ field: ConfigField, json: String) async {
        fieldErrors[field.key] = nil
        guard !ConfigJSON.same(json, field.value) else { return }
        if field.sensitive {
            confirmation = ConfigChange(field: field, json: json)
            return
        }
        await write(field.key, json: json, confirmed: false)
    }

    public func changeText(_ field: ConfigField, text: String) async {
        switch field.json(fromText: text) {
        case .success(let json): await change(field, json: json)
        case .failure(let problem): fieldErrors[field.key] = problem.message
        }
    }

    public func confirmChange() async {
        guard let change = confirmation else { return }
        confirmation = nil
        await write(change.field.key, json: change.json, confirmed: true)
    }

    public func cancelChange() { confirmation = nil }

    public func resetToDefault(_ field: ConfigField) async {
        await change(field, json: field.defaultValue)
    }

    private func write(_ key: String, json: String, confirmed: Bool) async {
        pending.insert(key)
        defer { pending.remove(key) }
        generation += 1
        do {
            state = try await api.setConfig([key: json], confirmSensitive: confirmed)
            fieldErrors[key] = nil
            lastError = nil
        } catch {
            fieldErrors[key] = displaySafe(DocsModel.refusal(error))
        }
    }

    /// Restarts orchd so waiting settings take effect, then reads what it runs.
    public func restart() async {
        guard !isRestarting else { return }
        isRestarting = true
        restartError = nil
        defer { isRestarting = false }
        do {
            try await restartDaemon()
        } catch {
            restartError = displaySafe(DocsModel.refusal(error))
            return
        }
        // The socket comes back a moment after the service manager returns.
        for _ in 0..<10 {
            await load()
            if lastError == nil { return }
            try? await Task.sleep(for: settleDelay)
        }
    }
}
