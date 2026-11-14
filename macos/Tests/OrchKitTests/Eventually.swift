import Foundation

/// Waits until `condition` holds, checking every 10 ms, for at most
/// `timeout`. Polling tests use it instead of a fixed sleep, because a busy
/// CI runner can start a poll loop late. The condition runs on the caller's
/// actor.
func eventually(
    timeout: Duration = .seconds(5),
    isolation: isolated (any Actor)? = #isolation,
    _ condition: () -> Bool
) async throws {
    let clock = ContinuousClock()
    let end = clock.now.advanced(by: timeout)
    while !condition(), clock.now < end {
        try await Task.sleep(for: .milliseconds(10))
    }
}
