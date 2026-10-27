import Foundation
import GRPCCore

/// The orchd calls behind settings views such as the Claude tab. Every call
/// names a provider and a kind, so a view over another tool's settings uses
/// the same calls.
public protocol SettingsAPI: Sendable {
    /// Every provider orchd manages, its kinds and its items.
    func settings() async throws -> SettingsListing
    /// Turns one item on or off; orchd writes the file at once.
    func setSetting(_ ref: SettingRef, enabled: Bool) async throws -> SettingItem
    /// Adds an item from an add form's fields, keyed by field name.
    func addSetting(provider: String, kind: String, fields: [String: String]) async throws -> SettingItem
    /// Removes an item you added or orch found.
    func removeSetting(_ ref: SettingRef) async throws
}

/// Names one item: its provider, its kind and its identity within them.
public struct SettingRef: Hashable, Sendable {
    public let provider: String
    public let kind: String
    public let identity: String

    public init(provider: String, kind: String, identity: String) {
        self.provider = provider
        self.kind = kind
        self.identity = identity
    }
}

/// Who put an item in its file.
public enum SettingOrigin: String, Sendable {
    /// Written by orch install: it can be turned off, not removed.
    case orch
    /// Added through orch.
    case you
    /// Seen in the file, put there by something else.
    case found

    public var label: String {
        switch self {
        case .orch: "orch"
        case .you: "you"
        case .found: "found"
        }
    }

    public var help: String {
        switch self {
        case .orch: "Added by orch install. Turning it off keeps it off; it cannot be removed."
        case .you: "Added here in Orch."
        case .found: "Found in the file, put there by another tool or by hand."
        }
    }
}

/// An item's state as orchd reports it.
public enum SettingStatus: String, Sendable {
    case active, disabled, removed, missing

    /// Why an item that is not simply on or off is in this state, or nil.
    public var note: String? {
        switch self {
        case .removed: "Removed from the file outside Orch. Turn it on to put it back."
        case .missing: "Missing from the file. orch install restores it, or turn it on here."
        case .active, .disabled: nil
        }
    }
}

/// One field of an add form, as the provider describes it.
public struct SettingField: Sendable, Equatable, Identifiable {
    public enum FieldType: String, Sendable { case string, text, int, bool, `enum` }

    public var id: String { name }
    public let name: String
    public let label: String
    public let type: FieldType
    public let required: Bool
    public let options: [String]
    public let help: String
    public let placeholder: String

    public init(name: String, label: String = "", type: FieldType = .string, required: Bool = false,
                options: [String] = [], help: String = "", placeholder: String = "") {
        self.name = name
        self.label = label.isEmpty ? name : label
        self.type = type
        self.required = required
        self.options = options
        self.help = help
        self.placeholder = placeholder
    }
}

/// A kind of item a provider manages, such as hooks or permission rules.
public struct SettingKind: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let title: String
    /// What an item's group is: an event, a list, a rulebook.
    public let groupTitle: String
    public let addable: Bool
    public let removable: Bool
    /// Off is recorded in the file (a plugin set to false) rather than by
    /// taking the item out of it.
    public let stateInFile: Bool
    public let fields: [SettingField]

    public init(name: String, title: String = "", groupTitle: String = "", addable: Bool = false, removable: Bool = false,
                stateInFile: Bool = false, fields: [SettingField] = []) {
        self.name = name
        self.title = title.isEmpty ? name : title
        self.groupTitle = groupTitle
        self.addable = addable
        self.removable = removable
        self.stateInFile = stateInFile
        self.fields = fields
    }
}

/// A tool whose settings orchd manages.
public struct SettingsProvider: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let title: String
    public let kinds: [SettingKind]
    public let files: [String]
    /// Why its files could not be read; its items are as last seen.
    public let error: String
    public let observedAt: Date?

    public init(name: String, title: String = "", kinds: [SettingKind] = [], files: [String] = [], error: String = "",
                observedAt: Date? = nil) {
        self.name = name
        self.title = title.isEmpty ? name : title
        self.kinds = kinds
        self.files = files
        self.error = error
        self.observedAt = observedAt
    }

    public func kind(_ name: String) -> SettingKind? { kinds.first { $0.name == name } }
}

/// One managed item.
public struct SettingItem: Sendable, Equatable, Identifiable {
    public var id: SettingRef { ref }
    public let ref: SettingRef
    /// The desired state.
    public let enabled: Bool
    public let origin: SettingOrigin
    public let status: SettingStatus
    public let group: String
    public let summary: String
    /// What orchd writes back to the file.
    public let payloadJSON: String
    public let createdAt: Date?
    public let updatedAt: Date?

    public init(ref: SettingRef, enabled: Bool = true, origin: SettingOrigin = .found, status: SettingStatus = .active,
                group: String = "", summary: String = "", payloadJSON: String = "{}", createdAt: Date? = nil,
                updatedAt: Date? = nil) {
        self.ref = ref
        self.enabled = enabled
        self.origin = origin
        self.status = status
        self.group = group
        self.summary = summary
        self.payloadJSON = payloadJSON
        self.createdAt = createdAt
        self.updatedAt = updatedAt
    }

    /// What the toggle shows: the desired state. A missing orch item reads
    /// on (it is wanted, and orch install restores it); a removed item off.
    public var isOn: Bool { enabled && status != .removed }

    /// Whether the toggle applies. A plugin or rule gone from its file has
    /// nothing left to turn on or off.
    public func canToggle(in kind: SettingKind?) -> Bool { !(status == .removed && (kind?.stateInFile ?? false)) }

    /// Whether Remove applies: the kind allows it and orch did not add it,
    /// or the item is already gone from its file and Remove only forgets it.
    public func canRemove(in kind: SettingKind?) -> Bool {
        status == .removed || ((kind?.removable ?? false) && origin != .orch)
    }

    /// The payload indented for the detail pane, made safe to show.
    public var prettyPayload: String {
        guard let data = payloadJSON.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let pretty = try? JSONSerialization.data(withJSONObject: object,
                                                       options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]),
              let text = String(data: pretty, encoding: .utf8) else { return displaySafe(payloadJSON) }
        return text.split(separator: "\n", omittingEmptySubsequences: false)
            .map { displaySafe(String($0)) }
            .joined(separator: "\n")
    }
}

/// Everything orchd manages, as of one look at the files.
public struct SettingsListing: Sendable, Equatable {
    public let providers: [SettingsProvider]
    public let items: [SettingItem]
    /// Changes whenever an item or a provider's state does.
    public let revision: Int64

    public init(providers: [SettingsProvider] = [], items: [SettingItem] = [], revision: Int64 = 0) {
        self.providers = providers
        self.items = items
        self.revision = revision
    }

    public func provider(_ name: String) -> SettingsProvider? { providers.first { $0.name == name } }
}

extension DaemonClient: SettingsAPI {
    public func settings() async throws -> SettingsListing {
        let response = try await orchdSettings()
        return Self.listing(response)
    }

    public func setSetting(_ ref: SettingRef, enabled: Bool) async throws -> SettingItem {
        var request = Orchd_V1_SetSettingEnabledRequest()
        request.provider = ref.provider
        request.kind = ref.kind
        request.identity = ref.identity
        request.enabled = enabled
        return Self.item(try await orchdSetSetting(request))
    }

    public func addSetting(provider: String, kind: String, fields: [String: String]) async throws -> SettingItem {
        var request = Orchd_V1_AddSettingRequest()
        request.provider = provider
        request.kind = kind
        request.fields = fields
        return Self.item(try await orchdAddSetting(request))
    }

    public func removeSetting(_ ref: SettingRef) async throws {
        var request = Orchd_V1_RemoveSettingRequest()
        request.provider = ref.provider
        request.kind = ref.kind
        request.identity = ref.identity
        try await orchdRemoveSetting(request)
    }

    static func listing(_ r: Orchd_V1_ListSettingsResponse) -> SettingsListing {
        SettingsListing(
            providers: r.providers.map { p in
                SettingsProvider(
                    name: p.name, title: p.title,
                    kinds: p.kinds.map { k in
                        SettingKind(
                            name: k.name, title: k.title, groupTitle: k.groupTitle, addable: k.addable,
                            removable: k.removable, stateInFile: k.stateInFile,
                            fields: k.fields.map {
                                SettingField(name: $0.name, label: $0.label, type: SettingField.FieldType(rawValue: $0.type) ?? .string,
                                             required: $0.required, options: $0.options, help: $0.help, placeholder: $0.placeholder)
                            })
                    },
                    files: p.files, error: p.error, observedAt: date(fromUnix: p.observedAt))
            },
            items: r.items.map(item), revision: r.revision)
    }

    static func item(_ i: Orchd_V1_SettingItem) -> SettingItem {
        SettingItem(
            ref: SettingRef(provider: i.provider, kind: i.kind, identity: i.identity), enabled: i.enabled,
            origin: SettingOrigin(rawValue: i.origin) ?? .found, status: SettingStatus(rawValue: i.status) ?? .active,
            group: i.group, summary: i.summary, payloadJSON: i.payloadJson,
            createdAt: date(fromUnix: i.createdAt), updatedAt: date(fromUnix: i.updatedAt))
    }
}
