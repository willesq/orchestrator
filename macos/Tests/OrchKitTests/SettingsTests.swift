import Foundation
import GRPCCore
import Testing
@testable import OrchKit

private struct Down: Error, CustomStringConvertible { var description: String { "RPCError: unavailable" } }

private struct Refused: Error, CustomStringConvertible {
    var description: String {
        #"RPCError(code: failedPrecondition, message: "orch install adds this hook, so it can be turned off but not removed")"#
    }
}

private let hookKind = SettingKind(
    name: "hook", title: "Hooks", groupTitle: "Event", addable: true, removable: true,
    fields: [
        SettingField(name: "event", label: "Event", type: .enum, required: true, options: ["SessionStart", "PreToolUse", "Stop"]),
        SettingField(name: "command", label: "Command", type: .text, required: true),
        SettingField(name: "timeout", label: "Timeout", type: .int),
        SettingField(name: "async", label: "Async", type: .bool),
    ])

private func ref(_ kind: String, _ id: String, provider: String = "claude-code") -> SettingRef {
    SettingRef(provider: provider, kind: kind, identity: id)
}

private func sampleListing(revision: Int64 = 1) -> SettingsListing {
    SettingsListing(
        providers: [
            SettingsProvider(name: "claude-code", title: "Claude Code", kinds: [
                hookKind, SettingKind(name: "plugin", title: "Plugins", stateInFile: true),
            ], files: ["/h/.claude/settings.json"]),
            SettingsProvider(name: "cc-safety-net", kinds: [SettingKind(name: "rule", stateInFile: true)], error: "parse rule.json"),
            SettingsProvider(name: "other"),
        ],
        items: [
            SettingItem(ref: ref("hook", "Stop/orch-hook stop"), origin: .orch, group: "Stop", summary: "orch-hook stop"),
            SettingItem(ref: ref("hook", "SessionStart/a"), enabled: false, origin: .you, status: .disabled,
                        group: "SessionStart", summary: "a"),
            SettingItem(ref: ref("hook", "Zed/b"), origin: .found, group: "Zed", summary: ""),
            SettingItem(ref: ref("hook", "Alpha/c"), origin: .found, status: .missing, group: "Alpha"),
            SettingItem(ref: ref("plugin", "p@m"), enabled: true, status: .removed, group: "m", summary: "p"),
        ],
        revision: revision)
}

private final class FakeSettings: SettingsAPI, @unchecked Sendable {
    private let lock = NSLock()
    var listing = sampleListing()
    var fail = false
    var changeError: (any Error)?
    private(set) var sets: [(SettingRef, Bool)] = []
    private(set) var adds: [[String: String]] = []
    private(set) var removes: [SettingRef] = []

    func settings() async throws -> SettingsListing {
        if fail { throw Down() }
        return lock.withLock { listing }
    }

    func setSetting(_ ref: SettingRef, enabled: Bool) async throws -> SettingItem {
        lock.withLock { sets.append((ref, enabled)) }
        if let changeError { throw changeError }
        return SettingItem(ref: ref, enabled: enabled, status: enabled ? .active : .disabled, summary: "done\u{1B}")
    }

    func addSetting(provider: String, kind: String, fields: [String: String]) async throws -> SettingItem {
        lock.withLock { adds.append(fields) }
        if let changeError { throw changeError }
        let item = SettingItem(ref: ref(kind, "new", provider: provider), origin: .you, summary: fields["command"] ?? "")
        lock.withLock {
            listing = SettingsListing(providers: listing.providers, items: listing.items + [item], revision: listing.revision + 1)
        }
        return item
    }

    func removeSetting(_ ref: SettingRef) async throws {
        lock.withLock { removes.append(ref) }
        if let changeError { throw changeError }
    }
}

@MainActor
struct SettingsModelTests {
    @Test func loadGroupsAndCounts() async {
        let api = FakeSettings()
        let model = SettingsModel(api: api, providers: ["cc-safety-net", "claude-code", "absent"])
        await model.load()
        #expect(model.providers.map(\.name) == ["cc-safety-net", "claude-code"])
        // Groups follow the event field's order, then the rest alphabetically.
        #expect(model.groups(provider: "claude-code", kind: "hook").map(\.name) == ["Alpha", "SessionStart", "Stop", "Zed"])
        let c = model.counts(provider: "claude-code", kind: "hook")
        #expect(c.on == 3 && c.total == 4)
        #expect(model.kind(of: ref("plugin", "p@m"))?.stateInFile == true)
        model.selection = ref("hook", "Zed/b")
        #expect(model.selected?.origin == .found)
        api.listing = SettingsListing(providers: api.listing.providers, items: [], revision: 2)
        await model.load()
        #expect(model.selection == nil)
        api.fail = true
        await model.load()
        #expect(model.lastError == "unavailable")
    }

    @Test func groupOrderUsesTheNamingField() async {
        let api = FakeSettings()
        api.listing = SettingsListing(providers: sampleListing().providers, items: [
            SettingItem(ref: ref("hook", "x"), group: "Stop"),
            SettingItem(ref: ref("hook", "y"), group: "PreToolUse"),
            SettingItem(ref: ref("hook", "z"), group: "SessionStart"),
        ])
        let model = SettingsModel(api: api, providers: ["claude-code"])
        await model.load()
        #expect(model.groups(provider: "claude-code", kind: "hook").map(\.name) == ["SessionStart", "PreToolUse", "Stop"])
    }

    @Test func toggleRecordsAndReloads() async {
        let api = FakeSettings()
        let model = SettingsModel(api: api, providers: ["claude-code"])
        await model.load()
        let item = model.items(provider: "claude-code", kind: "hook")[0]
        await model.setEnabled(item, false)
        #expect(api.sets.count == 1 && api.sets[0].0 == item.ref && api.sets[0].1 == false)
        #expect(model.message == "Turned off done\\u{001B}.")
        #expect(model.pending.isEmpty && model.itemErrors.isEmpty)
    }

    @Test func refusalsStayWithTheirItem() async {
        let api = FakeSettings()
        api.changeError = Refused()
        let model = SettingsModel(api: api, providers: ["claude-code"])
        await model.load()
        let item = model.items(provider: "claude-code", kind: "hook")[0]
        await model.remove(item)
        #expect(model.itemErrors[item.ref] == "orch install adds this hook, so it can be turned off but not removed")
        await model.setEnabled(item, true)
        #expect(model.itemErrors[item.ref] != nil)
        #expect(api.removes == [item.ref])
    }

    @Test func removeClearsTheSelection() async {
        let api = FakeSettings()
        let model = SettingsModel(api: api, providers: ["claude-code"])
        await model.load()
        let item = model.items(provider: "claude-code", kind: "hook")[1]
        model.selection = item.ref
        await model.remove(item)
        #expect(model.selection == nil && model.message == "Removed a.")
    }

    @Test func addValidatesThenSends() async {
        let api = FakeSettings()
        let model = SettingsModel(api: api, providers: ["claude-code"])
        var draft = SettingDraft(provider: "claude-code", kind: hookKind)
        #expect(draft.values["event"] == "SessionStart" && draft.values["async"] == "false")
        #expect(await model.add(draft) == "Command is required.")
        draft.values["command"] = "  ./check.sh  "
        draft.values["timeout"] = "soon"
        #expect(draft.problem == "Timeout must be a whole number.")
        draft.values["timeout"] = "30"
        #expect(draft.problem == nil)
        #expect(await model.add(draft) == nil)
        #expect(api.adds == [["event": "SessionStart", "command": "./check.sh", "timeout": "30", "async": "false"]])
        #expect(model.selection == ref("hook", "new") && model.message == "Added ./check.sh.")
        api.changeError = Refused()
        #expect(await model.add(draft) == "orch install adds this hook, so it can be turned off but not removed")
    }

    @Test func followStopsWhenCancelled() async {
        let api = FakeSettings()
        let model = SettingsModel(api: api, providers: ["claude-code"])
        let task = Task { await model.follow(interval: .milliseconds(10)) }
        try? await Task.sleep(for: .milliseconds(50))
        task.cancel()
        await task.value
        #expect(model.listing != nil)
    }
}

struct SettingItemTests {
    @Test func toggleAndRemoveRules() {
        let plugins = SettingKind(name: "plugin", stateInFile: true)
        let removed = SettingItem(ref: ref("plugin", "p"), enabled: true, status: .removed)
        #expect(!removed.isOn && !removed.canToggle(in: plugins) && removed.canToggle(in: hookKind))
        #expect(removed.canRemove(in: plugins))
        let missing = SettingItem(ref: ref("hook", "o"), enabled: true, origin: .orch, status: .missing)
        #expect(missing.isOn && !missing.canRemove(in: hookKind) && missing.status.note?.contains("orch install") == true)
        let mine = SettingItem(ref: ref("hook", "m"), origin: .you)
        #expect(mine.canRemove(in: hookKind) && !mine.canRemove(in: plugins) && !mine.canRemove(in: nil))
        #expect(SettingStatus.active.note == nil && SettingStatus.removed.note != nil)
        for origin in [SettingOrigin.orch, .you, .found] { #expect(!origin.label.isEmpty && !origin.help.isEmpty) }
    }

    @Test func prettyPayloadIsIndentedAndSafe() {
        let item = SettingItem(ref: ref("hook", "x"), payloadJSON: #"{"handler":{"command":"a\u001bb /x"},"event":"Stop"}"#)
        #expect(item.prettyPayload.contains("\"event\" : \"Stop\""))
        #expect(item.prettyPayload.contains("/x") && !item.prettyPayload.contains("\u{1B}"))
        #expect(SettingItem(ref: ref("hook", "y"), payloadJSON: "not json\u{7}").prettyPayload == "not json\\u{0007}")
    }

    @Test func defaults() {
        #expect(SettingField(name: "n").label == "n")
        #expect(SettingKind(name: "k").title == "k")
        #expect(SettingsProvider(name: "p").title == "p")
        #expect(SettingsListing().provider("x") == nil)
    }
}

@Suite struct SettingsRoundTripTests {
    @Test func listMapsEveryField() async throws {
        var field = Orchd_V1_SettingField()
        field.name = "event"
        field.label = "Event"
        field.type = "enum"
        field.required = true
        field.options = ["Stop"]
        field.help = "h"
        field.placeholder = "p"
        var kind = Orchd_V1_SettingKind()
        kind.name = "hook"
        kind.title = "Hooks"
        kind.groupTitle = "Event"
        kind.addable = true
        kind.removable = true
        kind.fields = [field]
        var provider = Orchd_V1_SettingsProvider()
        provider.name = "claude-code"
        provider.title = "Claude Code"
        provider.kinds = [kind]
        provider.files = ["/h/s.json"]
        provider.observedAt = 1_700_000_000
        var item = Orchd_V1_SettingItem()
        item.provider = "claude-code"
        item.kind = "hook"
        item.identity = "Stop/x"
        item.enabled = true
        item.origin = "orch"
        item.status = "missing"
        item.group = "Stop"
        item.summary = "x"
        item.payloadJson = "{}"
        item.updatedAt = 1_700_000_100
        var service = FakeOrchdService()
        service.settings.providers = [provider]
        service.settings.items = [item]
        service.settings.revision = 7
        service.settingItem = item
        try await withFakeOrchd(service) { socket in
            let client = try DaemonClient(socketPath: socket)
            let runner = Task { try? await client.run() }
            defer {
                client.close()
                runner.cancel()
            }
            let listing = try await client.settings()
            #expect(listing.revision == 7)
            let p = try #require(listing.provider("claude-code"))
            #expect(p.title == "Claude Code" && p.files == ["/h/s.json"] && p.observedAt != nil)
            let k = try #require(p.kind("hook"))
            #expect(k.addable && k.removable && k.groupTitle == "Event" && k.fields.first?.type == .enum)
            #expect(k.fields.first?.options == ["Stop"] && k.fields.first?.placeholder == "p")
            let i = try #require(listing.items.first)
            #expect(i.origin == .orch && i.status == .missing && i.updatedAt != nil && i.createdAt == nil)

            let r = SettingRef(provider: "claude-code", kind: "hook", identity: "Stop/x")
            _ = try await client.setSetting(r, enabled: false)
            let set = try #require(service.recorder.last(Orchd_V1_SetSettingEnabledRequest.self))
            #expect(set.identity == "Stop/x" && !set.enabled && set.provider == "claude-code")
            _ = try await client.addSetting(provider: "claude-code", kind: "permission", fields: ["list": "deny"])
            let add = try #require(service.recorder.last(Orchd_V1_AddSettingRequest.self))
            #expect(add.kind == "permission" && add.fields == ["list": "deny"])
            try await client.removeSetting(r)
            let rm = try #require(service.recorder.last(Orchd_V1_RemoveSettingRequest.self))
            #expect(rm.identity == "Stop/x")
        }
    }

    @Test func unknownValuesFallBack() {
        var item = Orchd_V1_SettingItem()
        item.origin = "alien"
        item.status = "weird"
        let mapped = DaemonClient.item(item)
        #expect(mapped.origin == .found && mapped.status == .active)
        var field = Orchd_V1_SettingField()
        field.type = "color"
        var kind = Orchd_V1_SettingKind()
        kind.fields = [field]
        var provider = Orchd_V1_SettingsProvider()
        provider.kinds = [kind]
        var response = Orchd_V1_ListSettingsResponse()
        response.providers = [provider]
        #expect(DaemonClient.listing(response).providers[0].kinds[0].fields[0].type == .string)
    }
}
