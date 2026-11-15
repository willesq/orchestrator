/// Builds each app feature's models the first time they are asked for and
/// keeps them for the app's life, so a feature added since SP-081 is held
/// through an accessor in its own file, not a stored property and an init
/// line in the app's shared model:
///
///     extension AppModel {
///         var chat: ChatModel { features.get { ChatModel(api: client.chat) } }
///     }
///
/// A feature that must run from launch (polling, notifications) still asks
/// for itself once in `AppModel.start()`.
@MainActor
public final class FeatureStore {
    private var built: [ObjectIdentifier: AnyObject] = [:]
    private var building: Set<ObjectIdentifier> = []

    public init() {}

    /// The one `F` this store holds, built by `build` on first use. `build`
    /// may ask for other features, but not, directly or through them, for
    /// `F` itself.
    public func get<F: AnyObject>(_ type: F.Type = F.self, build: () -> F) -> F {
        let key = ObjectIdentifier(type)
        if let feature = built[key] as? F { return feature }
        // Reason: a feature built while it was being built would be built
        // twice, leaving two copies of its state; fail where the cycle is.
        precondition(!building.contains(key), "\(type) needs itself to be built")
        building.insert(key)
        defer { building.remove(key) }
        let feature = build()
        built[key] = feature
        return feature
    }
}
