import OrchKit

// Direct-message search (SP-083, D-81), wired the SP-081 way: its own
// AppClient accessor, UnreachableDaemon conformance and FeatureStore model.

extension AppClient {
    /// orchd's operator-only SearchDirectMessages.
    var directMessages: any DirectMessagesAPI {
        switch self {
        case .live(let client): client
        case .unreachable(let standIn): standIn
        }
    }
}

extension UnreachableDaemon: DirectMessagesAPI {
    func searchDirectMessages(_ query: SearchQuery) async throws -> DirectMessageResult { throw failure }
}

extension AppModel {
    /// The Memory pane's Direct messages tab. It shows nothing while the
    /// Spaces pane's screen-share-safe mode is on.
    var directMessages: DirectMessageSearchModel {
        features.get { [client, spaces] in
            DirectMessageSearchModel(api: client.directMessages, isSharing: { spaces.shareSafe != nil })
        }
    }
}
