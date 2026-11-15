import OrchKit

/// orchd as the app reaches it: the live connection, or a stand-in that
/// fails every call when the transport cannot even be built.
///
/// A feature added since SP-081 reaches its API through an accessor in its
/// own file rather than by widening `core`, so features built in parallel add
/// files instead of editing one shared line:
///
///     extension AppClient {
///         var chat: any ChatAPI {
///             switch self {
///             case .live(let client): client
///             case .unreachable(let standIn): standIn
///             }
///         }
///     }
///     extension UnreachableDaemon: ChatAPI { ... throw failure ... }
///
/// with `extension DaemonClient: ChatAPI` in OrchKit. The compiler checks
/// that both cases conform.
enum AppClient: Sendable {
    case live(DaemonClient)
    case unreachable(UnreachableDaemon)

    /// Connects to orchd's socket and keeps the connection running.
    static func connect() -> AppClient {
        do {
            let client = try DaemonClient()
            Task.detached { try? await client.run() }
            return .live(client)
        } catch {
            return .unreachable(UnreachableDaemon(reason: error.localizedDescription))
        }
    }

    /// The APIs of the features that predate SP-081. Add none here: a new
    /// feature adds its own accessor (above).
    var core: any DaemonAPI & MemoryAPI & ControlAPI & TasksAPI & GitHubAPI & PRReviewAPI & PRLandingAPI & SystemAPI
        & CatalogAPI & SafetyNetAPI & SettingsAPI & ConfigAPI & DocsOwnersAPI & SpacesOverviewAPI & SpacesRelabelAPI {
        switch self {
        case .live(let client): client
        case .unreachable(let standIn): standIn
        }
    }
}
