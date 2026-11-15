import Foundation

/// The Spaces pane's "Relabel now" control (SP-079): orchd's Relabel RPC,
/// which orchd allows only for the operator.
extension DaemonClient: SpacesRelabelAPI {
    public func relabel(apply: Bool) async throws -> RelabelRun {
        var request = Orchd_V1_RelabelRequest()
        request.apply = apply
        let r = try await orchd.relabel(request)
        return RelabelRun(revision: r.revision, total: r.total, stale: r.stale, changed: r.changed, moved: r.moved,
                          failed: r.failed, applied: r.applied, error: r.error)
    }
}
