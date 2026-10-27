import Foundation
import Observation

/// Items of one kind that share a group: hooks of one event, rules of one
/// rulebook.
public struct SettingGroup: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let items: [SettingItem]
}

/// An add form being filled in, built from a kind's fields.
public struct SettingDraft: Sendable, Equatable {
    public let provider: String
    public let kind: SettingKind
    /// Values by field name, as typed.
    public var values: [String: String]

    public init(provider: String, kind: SettingKind) {
        self.provider = provider
        self.kind = kind
        var values: [String: String] = [:]
        for f in kind.fields {
            switch f.type {
            case .enum: values[f.name] = f.options.first ?? ""
            case .bool: values[f.name] = "false"
            default: values[f.name] = ""
            }
        }
        self.values = values
    }

    /// The values sent to orchd: trimmed, with empty ones left out.
    public var fields: [String: String] {
        var out: [String: String] = [:]
        for (k, v) in values {
            let t = v.trimmingCharacters(in: .whitespacesAndNewlines)
            if !t.isEmpty { out[k] = t }
        }
        return out
    }

    /// Why the form cannot be sent yet, or nil. orchd validates fully; this
    /// catches what the form itself can tell.
    public var problem: String? {
        let f = fields
        for field in kind.fields {
            let v = f[field.name]
            if field.required && v == nil { return "\(field.label) is required." }
            if field.type == .int, let v, Int(v) == nil { return "\(field.label) must be a whole number." }
        }
        return nil
    }
}

/// Drives a settings view over some of orchd's providers: listing their
/// items by kind and group, following changes made to the files, and
/// toggling, adding and removing items. Errors on one item are kept with
/// that item, so a view shows them inline.
@MainActor
@Observable
public final class SettingsModel {
    public private(set) var listing: SettingsListing?
    /// Why the last load failed.
    public private(set) var lastError: String?
    /// What the last change did.
    public private(set) var message: String?
    /// Items with a change in flight.
    public private(set) var pending: Set<SettingRef> = []
    /// Why the last change to an item failed.
    public private(set) var itemErrors: [SettingRef: String] = [:]
    public var selection: SettingRef?
    /// The providers shown, in order.
    public let providerNames: [String]

    private let api: any SettingsAPI
    /// Bumped per load, so an older reply cannot replace a newer one.
    private var generation = 0

    public init(api: any SettingsAPI, providers: [String]) {
        self.api = api
        providerNames = providers
    }

    /// The shown providers orchd reported, in the model's order.
    public var providers: [SettingsProvider] {
        providerNames.compactMap { listing?.provider($0) }
    }

    public func items(provider: String, kind: String) -> [SettingItem] {
        (listing?.items ?? []).filter { $0.ref.provider == provider && $0.ref.kind == kind }
    }

    /// A kind's items by group. Groups follow the order of the kind's enum
    /// field that names them (hook events in lifecycle order, permission
    /// lists), else alphabetical; items keep orchd's order.
    public func groups(provider: String, kind: String) -> [SettingGroup] {
        let items = items(provider: provider, kind: kind)
        let names = Array(Set(items.map(\.group)))
        let spec = listing?.provider(provider)?.kind(kind)
        let order = spec?.fields.first { $0.type == .enum && Set(names).isSubset(of: Set($0.options)) }?.options
        let sorted = names.sorted { a, b in
            if let order, let i = order.firstIndex(of: a), let j = order.firstIndex(of: b) { return i < j }
            return a.localizedCaseInsensitiveCompare(b) == .orderedAscending
        }
        return sorted.map { name in SettingGroup(name: name, items: items.filter { $0.group == name }) }
    }

    /// A kind's groups keeping only items whose display matches `query`;
    /// groups left empty are dropped.
    public func groups(provider: String, kind: String, matching query: String) -> [SettingGroup] {
        let all = groups(provider: provider, kind: kind)
        guard !query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return all }
        return all.compactMap { g in
            let kept = g.items.filter { $0.display.matches(query) || $0.group.localizedCaseInsensitiveContains(query) }
            return kept.isEmpty ? nil : SettingGroup(name: g.name, items: kept)
        }
    }

    /// Items of the shown providers that were removed or are missing from
    /// their files: edited outside orch.
    public var changedOutside: Int {
        (listing?.items ?? []).filter { item in
            providerNames.contains(item.ref.provider) && (item.status == .removed || item.status == .missing)
        }.count
    }

    /// "Hooks · 37 on" when every item is on, "Hooks · 35 of 37 on" otherwise,
    /// and the bare title when the kind has no items.
    public func countLabel(_ title: String, provider: String, kind: String) -> String {
        let c = counts(provider: provider, kind: kind)
        if c.total == 0 { return title }
        return c.on == c.total ? "\(title) · \(c.on) on" : "\(title) · \(c.on) of \(c.total) on"
    }

    /// How many of a kind's items are on, of how many.
    public func counts(provider: String, kind: String) -> (on: Int, total: Int) {
        let items = items(provider: provider, kind: kind)
        return (items.filter(\.isOn).count, items.count)
    }

    public var selected: SettingItem? {
        guard let selection else { return nil }
        return listing?.items.first { $0.ref == selection }
    }

    public func kind(of ref: SettingRef) -> SettingKind? { listing?.provider(ref.provider)?.kind(ref.kind) }

    public func load() async {
        generation += 1
        let mine = generation
        do {
            let fresh = try await api.settings()
            guard mine == generation else { return }
            if fresh != listing { listing = fresh }
            lastError = nil
            if let s = selection, !fresh.items.contains(where: { $0.ref == s }) { selection = nil }
        } catch {
            guard mine == generation else { return }
            lastError = DaemonMonitor.describe(error)
        }
    }

    /// Reloads until cancelled, so edits made to the files elsewhere show up
    /// while the view is open. orchd notices them within a couple of seconds.
    public func follow(interval: Duration = .seconds(2)) async {
        while !Task.isCancelled {
            await load()
            try? await Task.sleep(for: interval)
        }
    }

    /// Turns an item on or off.
    public func setEnabled(_ item: SettingItem, _ on: Bool) async {
        await change(item.ref) {
            let updated = try await self.api.setSetting(item.ref, enabled: on)
            self.message = "\(on ? "Turned on" : "Turned off") \(displaySafe(updated.summary.isEmpty ? updated.ref.identity : updated.summary))."
        }
    }

    /// Removes an item. Orch items are only turned off.
    public func remove(_ item: SettingItem) async {
        await change(item.ref) {
            try await self.api.removeSetting(item.ref)
            if self.selection == item.ref { self.selection = nil }
            self.message = "Removed \(displaySafe(item.summary.isEmpty ? item.ref.identity : item.summary))."
        }
    }

    /// Sends an add form. Returns the refusal to show in the form, or nil
    /// when the item was added.
    public func add(_ draft: SettingDraft) async -> String? {
        if let problem = draft.problem { return problem }
        do {
            let item = try await api.addSetting(provider: draft.provider, kind: draft.kind.name, fields: draft.fields)
            selection = item.ref
            message = "Added \(displaySafe(item.summary))."
            await load()
            return nil
        } catch {
            return DocsModel.refusal(error)
        }
    }

    private func change(_ ref: SettingRef, _ body: @MainActor () async throws -> Void) async {
        guard !pending.contains(ref) else { return }
        pending.insert(ref)
        itemErrors[ref] = nil
        message = nil
        defer { pending.remove(ref) }
        do {
            try await body()
        } catch {
            itemErrors[ref] = DocsModel.refusal(error)
        }
        await load()
    }
}
