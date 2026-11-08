import Foundation
import GRPCCore

/// orch's settings and the docs opt-in, for the Settings pane.
extension DaemonClient: ConfigAPI, DocsOwnersAPI {
    /// Listing repos waits on gh, and a purge may delete many docs.
    private var slowOptions: CallOptions {
        var options = CallOptions.defaults
        options.timeout = .seconds(120)
        return options
    }

    public func config() async throws -> ConfigState {
        Self.configState(try await orchd.getConfig(Orchd_V1_GetConfigRequest()))
    }

    public func setConfig(_ values: [String: String], confirmSensitive: Bool) async throws -> ConfigState {
        var request = Orchd_V1_SetConfigRequest()
        request.values = values
        request.confirmSensitive = confirmSensitive
        return Self.configState(try await orchd.setConfig(request))
    }

    public func removeLegacyConfig() async throws -> ConfigState {
        Self.configState(try await orchd.removeLegacyConfig(Orchd_V1_RemoveLegacyConfigRequest()))
    }

    static func configState(_ r: Orchd_V1_ConfigState) -> ConfigState {
        ConfigState(fields: r.fields.map { f in
            ConfigField(key: f.key, section: f.section, label: f.label, help: f.help,
                        type: ConfigFieldType(rawValue: f.type) ?? .string, value: f.value, defaultValue: f.defaultValue,
                        options: f.options, range: f.hasRange_p && f.min <= f.max ? f.min...f.max : nil,
                        sensitive: f.sensitive, live: f.live, pendingRestart: f.pendingRestart)
        }, restartRequired: r.restartRequired, legacyFile: r.legacyFile, startupError: r.startupError)
    }

    public func docsOwners() async throws -> [DocsOwner] {
        try await orchd.listDocsOwners(Orchd_V1_ListDocsOwnersRequest()).owners.map(Self.docsOwner)
    }

    static func docsOwner(_ o: Orchd_V1_DocOwner) -> DocsOwner {
        DocsOwner(owner: o.owner, enabled: o.enabled, repoMode: DocsRepoMode(rawValue: o.repoMode) ?? .all,
                  selectedRepos: o.selectedRepos, refreshSecs: o.refreshSecs,
                  lastSynced: o.lastSynced > 0 ? Date(timeIntervalSince1970: TimeInterval(o.lastSynced)) : nil,
                  status: o.status, error: o.error,
                  repos: o.repos.map { DocsOwnerRepoState(repo: $0.repo, files: Int($0.files), status: $0.status) })
    }

    public func docsOwnerRepos(owner: String) async throws -> [DocsOwnerRepoChoice] {
        var request = Orchd_V1_ListDocsOwnerReposRequest()
        request.owner = owner
        return try await orchd.listDocsOwnerRepos(request, options: slowOptions).repos.map {
            DocsOwnerRepoChoice(name: $0.name, isArchived: $0.archived, isFork: $0.fork)
        }
    }

    public func saveDocsOwner(owner: String, enabled: Bool, mode: DocsRepoMode, repos: [String]) async throws -> DocsOwner {
        var request = Orchd_V1_SaveDocsOwnerRequest()
        request.owner = owner
        request.enabled = enabled
        request.repoMode = mode.rawValue
        request.repos = mode == .all ? [] : repos
        return Self.docsOwner(try await orchd.saveDocsOwner(request))
    }

    public func removeDocsOwner(owner: String, purge: Bool) async throws -> DocsPurgeResult {
        var request = Orchd_V1_RemoveDocsOwnerRequest()
        request.owner = owner
        request.purge = purge
        request.confirm = purge
        return Self.purgeResult(try await orchd.removeDocsOwner(request, options: slowOptions))
    }

    public func purgeDocs(owner: String, repos: [String]) async throws -> DocsPurgeResult {
        var request = Orchd_V1_PurgeDocsRequest()
        request.owner = owner
        request.repos = repos
        request.confirm = true
        return Self.purgeResult(try await orchd.purgeDocs(request, options: slowOptions))
    }

    private static func purgeResult(_ r: Orchd_V1_PurgeDocsResponse) -> DocsPurgeResult {
        DocsPurgeResult(sources: Int(r.purgedSources), chunks: Int(r.purgedChunks))
    }
}
