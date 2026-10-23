import Foundation

/// Starts and restarts orchd through the `orch` CLI, which already knows
/// whether brew services or launchd manages it.
public struct DaemonControl: Sendable {
    public enum Action: String, Sendable {
        case start, restart
    }

    public enum ControlError: Error, Equatable, LocalizedError {
        case orchNotFound
        case failed(status: Int32, output: String)

        public var errorDescription: String? {
            switch self {
            case .orchNotFound: "Could not find the orch CLI. Install it with: brew install willesq/tap/orch"
            case .failed(let status, let output): "orch exited \(status): \(output)"
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

    /// Runs `orch daemon <action>` and waits for it.
    public func perform(_ action: Action) async throws {
        guard let orchPath else { throw ControlError.orchNotFound }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: orchPath)
        process.arguments = Self.arguments(for: action)
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe

        // Buffered, so an exit before anyone awaits it is not lost.
        let exited = AsyncStream<Void> { continuation in
            process.terminationHandler = { _ in
                continuation.yield()
                continuation.finish()
            }
        }
        try process.run()
        // Drain the pipe while the child runs: once its 64 KiB buffer fills, a
        // chatty child blocks on write and never exits. EOF arrives when it does.
        let data = await Task.detached { pipe.fileHandleForReading.readDataToEndOfFile() }.value
        for await _ in exited {}
        guard process.terminationStatus == 0 else {
            let output = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw ControlError.failed(status: process.terminationStatus, output: output)
        }
    }
}
