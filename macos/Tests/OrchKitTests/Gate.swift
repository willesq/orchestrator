/// Holds a fake's call in flight until the test opens it. Tests that need a
/// call still running while they do something else use it instead of a
/// fixed delay, which a busy CI runner can outlast before the test gets
/// there. One call may wait at a time; once open, waits return at once.
final class Gate: Sendable {
    private let stream: AsyncStream<Void>
    private let opener: AsyncStream<Void>.Continuation

    init() {
        (stream, opener) = AsyncStream.makeStream(of: Void.self)
    }

    func wait() async {
        for await _ in stream {}
    }

    func open() {
        opener.finish()
    }
}
