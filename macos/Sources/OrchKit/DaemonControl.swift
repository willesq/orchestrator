import Foundation

/// Starts and restarts orchd through the `orch` CLI, which already knows
/// whether brew services or launchd manages it, and reads its status report.
public struct DaemonControl: Sendable {
    public enum Action: String, Sendable {
        case start, restart
    }

    public enum ControlError: Error, Equatable, LocalizedError {
        case orchNotFound
        case failed(status: Int32, output: String)
        case timedOut
        case unreadable

        public var errorDescription: String? {
            switch self {
            case .orchNotFound: "Could not find the orch CLI. Install it with: brew install willesq/tap/orch"
            case .failed(let status, let output): "orch exited \(status): \(output)"
            case .timedOut: "orch did not finish in time and was stopped"
            case .unreadable: "Could not read orch's report; update orch (brew upgrade orch)"
            }
        }
    }

    /// An app launched from Finder gets launchd's minimal PATH, so look in the
    /// usual install locations rather than relying on it.
    public static func candidatePaths(home: String = NSHomeDirectory()) -> [String] {
        [
            "/opt/homebrew/bin/orch",
            "/usr/local/bin/orch",
            (home as NSString).appendingPathComponent(".local/bin/orch"),
            (home as NSString).appendingPathComponent("go/bin/orch"),
        ]
    }

    public let orchPath: String?

    public init(orchPath: String? = nil, isExecutable: (String) -> Bool = FileManager.default.isExecutableFile(atPath:)) {
        self.orchPath = orchPath ?? Self.candidatePaths().first(where: isExecutable)
    }

    /// The argv `perform` runs, without the executable.
    public static func arguments(for action: Action) -> [String] {
        ["daemon", action.rawValue]
    }

    /// Runs `orch daemon <action>` and waits for it. A brew services restart
    /// can be slow, so it gets two minutes.
    public func perform(_ action: Action) async throws {
        let result = try await run(Self.arguments(for: action), timeout: .seconds(120))
        guard result.status == 0 else {
            throw ControlError.failed(status: result.status, output: Self.message(result.stdout + result.stderr))
        }
    }

    /// Runs `orch status --json`, which reports every component, including
    /// the checks that work with orchd down.
    public func statusReport(timeout: Duration = .seconds(30)) async throws -> StatusReport {
        let result = try await run(["status", "--json"], timeout: timeout)
        guard result.status == 0 else {
            throw ControlError.failed(status: result.status, output: Self.message(result.stderr))
        }
        do {
            return try JSONDecoder().decode(StatusReport.self, from: result.stdout)
        } catch {
            throw ControlError.unreadable
        }
    }

    /// Output shown in an error, trimmed and capped so a noisy child cannot
    /// flood the window.
    static func message(_ data: Data) -> String {
        let text = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        return text.count > 500 ? String(text.prefix(500)) + "…" : text
    }

    /// Runs orch from an argument array, never a shell, and waits for it. Past
    /// `timeout`, or when the caller is cancelled, the child is terminated
    /// (then killed), so a hung orch cannot leave the caller waiting for good.
    private func run(_ arguments: [String], timeout: Duration) async throws -> (status: Int32, stdout: Data, stderr: Data) {
        guard let orchPath else { throw ControlError.orchNotFound }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: orchPath)
        process.arguments = arguments
        let out = Pipe(), err = Pipe()
        process.standardOutput = out
        process.standardError = err

        // Buffered, so an exit before anyone awaits it is not lost.
        let exited = AsyncStream<Void> { continuation in
            process.terminationHandler = { _ in
                continuation.yield()
                continuation.finish()
            }
        }
        try process.run()
        let expired = Flag()
        let watchdog = Task.detached {
            try await Task.sleep(for: timeout)
            expired.set()
            Self.end(process)
        }
        defer { watchdog.cancel() }
        return try await withTaskCancellationHandler {
            // Drain both pipes while the child runs: once a 64 KiB buffer fills,
            // a chatty child blocks on write and never exits. The blocking reads
            // run on a dispatch queue, not the cooperative pool, and are not
            // awaited after a timeout: a grandchild holding the pipes open
            // would otherwise keep the caller waiting.
            let stdout = Task { await Self.readToEnd(out.fileHandleForReading) }
            let stderr = Task { await Self.readToEnd(err.fileHandleForReading) }
            for await _ in exited {}
            if expired.isSet { throw ControlError.timedOut }
            try Task.checkCancellation()
            return (process.terminationStatus, await stdout.value, await stderr.value)
        } onCancel: {
            Self.end(process)
        }
    }

    private static func end(_ process: Process) {
        guard process.isRunning else { return }
        process.terminate()
        let pid = process.processIdentifier
        DispatchQueue.global().asyncAfter(deadline: .now() + 2) {
            if process.isRunning { kill(pid, SIGKILL) }
        }
    }

    private static func readToEnd(_ handle: FileHandle) async -> Data {
        await withCheckedContinuation { continuation in
            DispatchQueue.global().async { continuation.resume(returning: handle.readDataToEndOfFile()) }
        }
    }
}

/// A one-way flag set from the watchdog and read once the child has exited.
private final class Flag: @unchecked Sendable {
    private let lock = NSLock()
    private var value = false
    func set() { lock.withLock { value = true } }
    var isSet: Bool { lock.withLock { value } }
}
