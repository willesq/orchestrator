import OrchKit

/// The advanced editors' API (SP-053): the live client or the stand-in.
extension AppClient {
    var spacesEditor: any SpacesEditorAPI {
        switch self {
        case .live(let client): client
        case .unreachable(let standIn): standIn
        }
    }
}

/// The editors' own calls; the shared ones come from the stand-in's other
/// conformances.
extension UnreachableDaemon: SpacesEditorAPI {
    func spaceTerms() async throws -> [SpaceTermsInfo] { throw failure }
    func setSpaceTerms(space: String, terms: String) async throws -> SpaceTermsInfo { throw failure }
    func quarantinedSources() async throws -> [QuarantinedSource] { throw failure }
    func retagSource(uri: String, space: String, confirm: Bool, expectFrom: String) async throws -> RetagOutcome { throw failure }
    func spaceChunkCounts() async throws -> [SpaceChunkCount] { throw failure }
}

extension AppModel {
    /// The Spaces pane's Edit page, built when the window first asks.
    var spacesEditor: SpacesEditorModel { features.get { SpacesEditorModel(api: client.spacesEditor) } }
}
