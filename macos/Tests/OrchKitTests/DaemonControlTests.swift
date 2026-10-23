import Foundation
import Testing
@testable import OrchKit

@Suite struct DaemonControlTests {
    @Test func picksFirstExecutableCandidate() {
        let home = NSHomeDirectory()
        let control = DaemonControl(isExecutable: { $0 == "\(home)/.local/bin/orch" || $0 == "\(home)/go/bin/orch" })
        #expect(control.orchPath == "\(home)/.local/bin/orch")
    }

    @Test func explicitPathWins() {
        #expect(DaemonControl(orchPath: "/x/orch", isExecutable: { _ in false }).orchPath == "/x/orch")
    }

    @Test func homebrewFirst() {
        #expect(DaemonControl.candidatePaths(home: "/Users/u").first == "/opt/homebrew/bin/orch")
    }

    @Test func arguments() {
        #expect(DaemonControl.arguments(for: .start) == ["daemon", "start"])
        #expect(DaemonControl.arguments(for: .restart) == ["daemon", "restart"])
    }

    @Test func missingOrchThrows() async {
        let control = DaemonControl(isExecutable: { _ in false })
        await #expect(throws: DaemonControl.ControlError.orchNotFound) {
            try await control.perform(.start)
        }
    }

    @Test func runsOrchWithArguments() async throws {
        let dir = try TempDir()
        let log = dir.url.appendingPathComponent("argv").path
        let orch = try dir.script("echo \"$@\" > '\(log)'")
        try await DaemonControl(orchPath: orch).perform(.restart)
        #expect(try String(contentsOfFile: log, encoding: .utf8) == "daemon restart\n")
    }

    @Test func nonzeroExitCarriesOutput() async throws {
        let dir = try TempDir()
        let orch = try dir.script("echo 'brew services failed' >&2; exit 3")
        await #expect(throws: DaemonControl.ControlError.failed(status: 3, output: "brew services failed")) {
            try await DaemonControl(orchPath: orch).perform(.start)
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func largeOutputDoesNotDeadlock() async throws {
        let dir = try TempDir()
        // Well past a pipe's 64 KiB buffer, so the child blocks unless the
        // parent drains the pipe while it runs.
        let orch = try dir.script("head -c 300000 /dev/zero | tr '\\0' x; exit 4")
        do {
            try await DaemonControl(orchPath: orch).perform(.start)
            Issue.record("expected a failure")
        } catch let DaemonControl.ControlError.failed(status, output) {
            #expect(status == 4)
            #expect(output.count == 300_000)
        }
    }

    @Test func unlaunchablePathThrows() async throws {
        let dir = try TempDir()
        let path = dir.url.appendingPathComponent("not-executable").path
        FileManager.default.createFile(atPath: path, contents: Data("x".utf8))
        await #expect(throws: (any Error).self) {
            try await DaemonControl(orchPath: path).perform(.start)
        }
    }

    @Test func errorDescriptions() {
        #expect(DaemonControl.ControlError.orchNotFound.errorDescription?.contains("brew install willesq/tap/orch") == true)
        #expect(DaemonControl.ControlError.failed(status: 2, output: "boom").errorDescription == "orch exited 2: boom")
    }
}

/// A scratch directory removed when the value is released. It lives under
/// /tmp because macOS's per-user temporary directory is long enough to push a
/// socket path inside it past the 104-byte Unix socket limit.
final class TempDir {
    let url: URL

    init() throws {
        url = URL(fileURLWithPath: "/tmp").appendingPathComponent("ok-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    }

    deinit { try? FileManager.default.removeItem(at: url) }

    /// Writes an executable shell script standing in for `orch`.
    func script(_ body: String) throws -> String {
        let path = url.appendingPathComponent("orch").path
        try "#!/bin/sh\n\(body)\n".write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
        return path
    }
}
