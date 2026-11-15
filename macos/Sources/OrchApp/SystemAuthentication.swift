import Foundation
import LocalAuthentication
import OrchKit

/// Touch ID or the login password, through the system's authentication
/// sheet, before the Review page applies a spaces policy.
struct SystemAuthentication: OperatorAuthenticator {
    struct Unavailable: LocalizedError {
        let reason: String
        var errorDescription: String? { reason }
    }

    func confirm(reason: String) async throws {
        let context = LAContext()
        var error: NSError?
        // deviceOwnerAuthentication offers Touch ID where it is set up and
        // falls back to the login password.
        guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else {
            throw Unavailable(reason: error?.localizedDescription ?? "System authentication is not available.")
        }
        guard try await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason) else {
            throw CancellationError()
        }
    }
}
